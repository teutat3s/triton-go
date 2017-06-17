package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/joyent/triton-go/authentication"
)

const nilContext = "nil context"

var MissingKeyIdError = errors.New("Default SSH agent authentication requires SDC_KEY_ID")

// Client represents a connection to the Triton API.
type Client struct {
	HTTPClient  *http.Client
	Authorizers []authentication.Signer
	APIURL      url.URL
	AccountName string
	Endpoint    string
}

type Config struct {
	endpoint    string
	accountName string
	signers     []authentication.Signer
}

type ClientError struct {
	StatusCode int
	Code       string
	Message    string
}

// Error implements interface Error on the TritonError type.
func (e ClientError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New is used to construct a Client in order to make API
// requests to the Triton API.
//
// At least one signer must be provided - example signers include
// authentication.PrivateKeySigner and authentication.SSHAgentSigner.
func New(endpoint string, accountName string, signers ...authentication.Signer) (*Client, error) {
	apiURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, errwrap.Wrapf("invalid endpoint: {{err}}", err)
	}

	if accountName == "" {
		return nil, errors.New("account name can not be empty")
	}

	httpClient := &http.Client{
		Transport:     httpTransport(false),
		CheckRedirect: doNotFollowRedirects,
	}

	newClient := &Client{
		HTTPClient:  httpClient,
		Authorizers: signers,
		APIURL:      *apiURL,
		AccountName: accountName,
		Endpoint:    endpoint,
	}

	var authorizers []authentication.Signer
	for _, key := range signers {
		if key != nil {
			authorizers = append(authorizers, key)
		}
	}

	// Default to constructing an SSHAgentSigner if there are no other signers
	// passed into NewClient and there's an SDC_KEY_ID value available in the
	// user environ.
	if len(authorizers) == 0 {
		keyID := os.Getenv("SDC_KEY_ID")
		if len(keyID) != 0 {
			keySigner, err := authentication.NewSSHAgentSigner(keyID, accountName)
			if err != nil {
				return nil, errwrap.Wrapf("Problem initializing NewSSHAgentSigner: {{err}}", err)
			}
			newClient.Authorizers = append(authorizers, keySigner)
		} else {
			return nil, MissingKeyIdError
		}
	}

	return newClient, nil
}

// InsecureSkipTLSVerify turns off TLS verification for the client connection. This
// allows connection to an endpoint with a certificate which was signed by a non-
// trusted CA, such as self-signed certificates. This can be useful when connecting
// to temporary Triton installations such as Triton Cloud-On-A-Laptop.
func (c *Client) InsecureSkipTLSVerify() {
	if c.HTTPClient == nil {
		return
	}

	c.HTTPClient.Transport = httpTransport(true)
}

func httpTransport(insecureSkipTLSVerify bool) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: -1,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecureSkipTLSVerify,
		},
	}
}

func doNotFollowRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (c *Client) FormatURL(path string) string {
	return fmt.Sprintf("%s%s", c.Endpoint, path)
}

func (c *Client) DecodeError(statusCode int, body io.Reader) error {
	err := &ClientError{
		StatusCode: statusCode,
	}

	errorDecoder := json.NewDecoder(body)
	if err := errorDecoder.Decode(err); err != nil {
		return errwrap.Wrapf("Error decoding error response: {{err}}", err)
	}

	return err
}

// -----------------------------------------------------------------------------

func (c *Client) ExecuteRequestURIParams(ctx context.Context, method, path string, body interface{}, query *url.Values) (io.ReadCloser, error) {
	var requestBody io.ReadSeeker
	if body != nil {
		marshaled, err := json.MarshalIndent(body, "", "    ")
		if err != nil {
			return nil, err
		}
		requestBody = bytes.NewReader(marshaled)
	}

	endpoint := c.APIURL
	endpoint.Path = path
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}

	req, err := http.NewRequest(method, endpoint.String(), requestBody)
	if err != nil {
		return nil, errwrap.Wrapf("Error constructing HTTP request: {{err}}", err)
	}

	dateHeader := time.Now().UTC().Format(time.RFC1123)
	req.Header.Set("date", dateHeader)

	// NewClient ensures there's always an authorizer (unless this is called
	// outside that constructor).
	authHeader, err := c.Authorizers[0].Sign(dateHeader)
	if err != nil {
		return nil, errwrap.Wrapf("Error signing HTTP request: {{err}}", err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Version", "8")
	req.Header.Set("User-Agent", "triton-go Client API")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, errwrap.Wrapf("Error executing HTTP request: {{err}}", err)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp.Body, nil
	}

	return nil, c.DecodeError(resp.StatusCode, resp.Body)
}

func (c *Client) ExecuteRequest(ctx context.Context, method, path string, body interface{}) (io.ReadCloser, error) {
	return c.ExecuteRequestURIParams(ctx, method, path, body, nil)
}

func (c *Client) ExecuteRequestRaw(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var requestBody io.ReadSeeker
	if body != nil {
		marshaled, err := json.MarshalIndent(body, "", "    ")
		if err != nil {
			return nil, err
		}
		requestBody = bytes.NewReader(marshaled)
	}

	endpoint := c.APIURL
	endpoint.Path = path

	req, err := http.NewRequest(method, endpoint.String(), requestBody)
	if err != nil {
		return nil, errwrap.Wrapf("Error constructing HTTP request: {{err}}", err)
	}

	dateHeader := time.Now().UTC().Format(time.RFC1123)
	req.Header.Set("date", dateHeader)

	// NewClient ensures there's always an authorizer (unless this is called
	// outside that constructor).
	authHeader, err := c.Authorizers[0].Sign(dateHeader)
	if err != nil {
		return nil, errwrap.Wrapf("Error signing HTTP request: {{err}}", err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Version", "8")
	req.Header.Set("User-Agent", "triton-go c API")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, errwrap.Wrapf("Error executing HTTP request: {{err}}", err)
	}

	return resp, nil
}

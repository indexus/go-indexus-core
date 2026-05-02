package peer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// httpBaseURL returns "http://host:port" for dialing `host`, bracketing it
// when this contact's primary address (c.ip) is IPv6 — matching the legacy
// behavior used across all P2P calls.
func (c *Contact) httpBaseURL(host string) string {
	displayHost := host
	if parsed := net.ParseIP(c.ip); parsed != nil && parsed.To4() == nil {
		displayHost = fmt.Sprintf("[%s]", host)
	}
	return fmt.Sprintf("http://%s:%d", displayHost, c.port)
}

// url builds http://{c.ip}:port/path[?query] with the same IPv6 bracket rule.
func (c *Contact) url(path string, query url.Values) string {
	s := c.httpBaseURL(c.ip) + path
	if len(query) > 0 {
		s += "?" + query.Encode()
	}
	return s
}

// jsonRoundTrip issues an HTTP request. When reqBody is non-nil it is JSON
// marshaled and Content-Type is set. When respBody is non-nil the response
// entity is JSON-decoded into it (caller must check StatusCode when needed).
func jsonRoundTrip(client *http.Client, method, urlStr string, reqBody, respBody any) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		return 0, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	st := resp.StatusCode
	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return st, err
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return st, nil
}

// doRaw performs a GET (or any method with no body) and returns the full body.
func doRaw(client *http.Client, method, urlStr string) ([]byte, int, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

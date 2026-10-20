package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(endpoint, token string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid API URL", "Use an origin URL without credentials, query, or path.")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, contract.NewFault(contract.ValidationFailed, "API transport must be encrypted", "Use HTTPS; plain HTTP is allowed only for literal loopback development hosts.")
	}
	if !tokenPattern.MatchString(token) {
		return nil, contract.NewFault(contract.InputRequired, "a valid EREGION_API_TOKEN is required", "Provide a random 32–256 character API token through the environment, not command arguments.")
	}
	return &Client{base: strings.TrimSuffix(u.String(), "/"), token: token, http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Test(ctx context.Context, input operations.TestInput, key string, dryRun bool) (json.RawMessage, error) {
	raw, _ := json.Marshal(input)
	result, err := c.request(ctx, http.MethodPost, "/v1/operations", testRequest{Kind: operations.TestKind, Target: operations.TestTarget, Input: raw, DryRun: dryRun}, key)
	if err != nil {
		return nil, err
	}
	if dryRun {
		var preview operations.TestPreview
		if json.Unmarshal(result, &preview) != nil || !preview.DryRun || preview.Kind != operations.TestKind || preview.Target != operations.TestTarget {
			return nil, invalidData()
		}
		return result, nil
	}
	return validOperationResult(result, "")
}

func (c *Client) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	result, err := c.request(ctx, http.MethodGet, "/v1/operations/"+string(id), nil, "")
	if err != nil {
		return nil, err
	}
	return validOperationResult(result, id)
}

func (c *Client) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	if err := cursor.OperationID.Validate(); err != nil {
		return nil, err
	}
	query := url.Values{"after": {strconv.FormatUint(cursor.After, 10)}, "limit": {strconv.Itoa(limit)}}
	result, err := c.request(ctx, http.MethodGet, "/v1/operations/"+string(cursor.OperationID)+"/events?"+query.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	var page operations.EventPage
	if json.Unmarshal(result, &page) != nil || page.Events == nil || len(page.Events) > limit || page.Cursor.OperationID != cursor.OperationID {
		return nil, invalidData()
	}
	after := cursor.After
	for _, event := range page.Events {
		if event.Validate() != nil || event.OperationID != cursor.OperationID || event.Sequence != after+1 {
			return nil, invalidData()
		}
		after = event.Sequence
	}
	if page.Cursor.After != after {
		return nil, invalidData()
	}
	return result, nil
}

func (c *Client) Cancel(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	result, err := c.request(ctx, http.MethodPost, "/v1/operations/"+string(id)+"/cancel", nil, "")
	if err != nil {
		return nil, err
	}
	return validOperationResult(result, id)
}

func validOperationResult(raw json.RawMessage, id contract.OperationID) (json.RawMessage, error) {
	var op contract.Operation
	if json.Unmarshal(raw, &op) != nil || op.Validate() != nil || (id != "" && op.ID != id) {
		return nil, invalidData()
	}
	return raw, nil
}

func invalidData() *contract.Fault {
	return contract.NewFault(contract.Unavailable, "API returned invalid operation data", "Check binary compatibility; do not assume an unrecognized result means work was not accepted.")
}

func (c *Client) request(ctx context.Context, method, path string, body any, key string) (json.RawMessage, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, contract.NewFault(contract.Unavailable, "API request did not complete", "Inspect the existing operation or reuse the same submission key; a timed-out request may have committed.")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1048577))
	if err != nil || len(raw) > 1048576 {
		return nil, contract.NewFault(contract.Unavailable, "API response is incomplete or too large", "Reconnect to the existing operation; do not blindly resubmit.")
	}
	var envelope struct {
		Schema  string          `json:"schema"`
		OK      bool            `json:"ok"`
		Command string          `json:"command"`
		Data    json.RawMessage `json:"data"`
		Error   *contract.Fault `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Schema != contract.Schema || envelope.Command == "" {
		return nil, contract.NewFault(contract.Unavailable, "unsupported or invalid API response", "Check the API endpoint and binary compatibility; redirects are not followed.")
	}
	if !envelope.OK && envelope.Error != nil && contract.KnownCode(envelope.Error.Code) && envelope.Error.Message != "" {
		return nil, envelope.Error
	}
	if !envelope.OK || envelope.Error != nil || len(envelope.Data) == 0 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, contract.NewFault(contract.Unavailable, "API result and HTTP status are inconsistent", "Inspect the existing operation before retrying a submission.")
	}
	return envelope.Data, nil
}

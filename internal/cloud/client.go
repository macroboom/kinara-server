package cloud

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client calls kinora-cloud's API.
type client struct {
	base string
	hc   *http.Client
}

// apiError is an error answer from the cloud.
type apiError struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("cloud answered %d", e.Status)
}

func isUnlinked(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && ae.Code == "unlinked"
}

func (c *client) do(ctx context.Context, method, path, token string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "kinora-server")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var e struct{ Error, Code string }
		json.Unmarshal(b, &e)
		return resp.StatusCode, &apiError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("cloud answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

type linkStart struct {
	Code              string `json:"code"`
	PollToken         string `json:"pollToken"`
	VerifyURL         string `json:"verifyUrl"`
	VerifyURLComplete string `json:"verifyUrlComplete"`
	ExpiresIn         int    `json:"expiresIn"`
	Interval          int    `json:"interval"`
}

// serverInfo is the cloud's description of this server's names; it comes
// back from the link poll, /v1/server/me and every presence report.
type serverInfo struct {
	ServerID     string   `json:"serverId"`
	ServerSecret string   `json:"serverSecret"`
	Account      Owner    `json:"account"`
	Owner        *Owner   `json:"owner"`
	DirectZone   string   `json:"directZone"`
	CertName     string   `json:"certName"`
	RelayHost    string   `json:"relayHost"`
	IrohRelays   []string `json:"irohRelays"`
	CloudKeyID   string   `json:"cloudKeyId"`
	CloudKey     string   `json:"cloudKey"`
	// Presence answers.
	ObservedAddr   string   `json:"observedAddr"`
	PublicV4       string   `json:"publicV4"`
	Direct4OK      bool     `json:"direct4ok"`
	Direct6OK      []string `json:"direct6ok"`
	RelayConnected bool     `json:"relayConnected"`
}

type presenceOut struct {
	Version    string   `json:"version"`
	Name       string   `json:"name"`
	LocalAddrs []string `json:"localAddrs"`
	Port       int      `json:"port"`
	HTTPSPort  int      `json:"httpsPort"`
	CertReady  bool     `json:"certReady"`
	IPv6       []string `json:"ipv6"`
	PublicV4   string   `json:"publicV4"`
	MappedPort int      `json:"mappedPort"`
	IrohTicket string   `json:"irohTicket"`
	IrohNode   string   `json:"irohNode"`
}

// accessClaims is what a cloud access token says (see kinora-cloud's
// tokens package).
type accessClaims struct {
	Type      string `json:"typ"`
	Subject   string `json:"sub"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Server    string `json:"sid"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"exp"`
	KeyID     string `json:"kid"`
}

// verifyAccess checks a "kc1." token against the cloud's public key.
func verifyAccess(token, pubB64 string, now time.Time) (accessClaims, error) {
	var c accessClaims
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return c, errors.New("no cloud key yet")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "kc1" {
		return c, errors.New("not a Kinara account token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), []byte(parts[0]+"."+parts[1]), sig) {
		return c, errors.New("the account token isn't valid")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return c, err
	}
	if now.Unix() > c.ExpiresAt {
		return c, errors.New("the account token has expired")
	}
	if c.Type != "server-access" {
		return c, errors.New("wrong kind of token")
	}
	return c, nil
}

package oauth

// WHY a custom mechanism: go-sasl ships OAUTHBEARER but not XOAUTH2, and
// Microsoft / Google IMAP still advertise the legacy XOAUTH2 capability.
// The wire format is a stable one-liner — inlining it here keeps the
// external SASL surface unchanged and avoids a fork.

import (
	"errors"

	"github.com/emersion/go-sasl"
)

// xoauth2Mech is the SASL mechanism name servers advertise as
// AUTH=XOAUTH2.
const xoauth2Mech = "XOAUTH2"

// xoauth2Client implements sasl.Client for the XOAUTH2 mechanism per
// the Google / Microsoft spec.
type xoauth2Client struct {
	username    string
	accessToken string
}

// XOAUTH2Client builds a SASL XOAUTH2 client per Google/Microsoft's
// widely-implemented spec: initial response is
// "user=<username>\x01auth=Bearer <token>\x01\x01". Servers that reject
// send a JSON error challenge; we return an empty response to abort so
// go-imap surfaces the LOGIN error verbatim.
func XOAUTH2Client(username, accessToken string) sasl.Client {
	return &xoauth2Client{username: username, accessToken: accessToken}
}

func (c *xoauth2Client) Start() (mech string, ir []byte, err error) {
	if c.username == "" || c.accessToken == "" {
		return "", nil, errors.New("oauth: xoauth2 requires username + access token")
	}
	ir = []byte("user=" + c.username + "\x01auth=Bearer " + c.accessToken + "\x01\x01")
	return xoauth2Mech, ir, nil
}

// Next is only reached when the server rejected the initial response
// with a JSON error challenge. Returning empty completes the SASL
// exchange so the underlying IMAP client surfaces the AUTHENTICATE
// failure with the server's diagnostic verbatim, rather than us
// second-guessing its shape.
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return []byte{}, nil
}

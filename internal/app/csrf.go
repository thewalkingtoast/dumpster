package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// csrfTTL bounds how long a token minted into a page stays usable. Dumpsters are
// ephemeral and so are the forms that rent them, so this only needs to outlive a
// page someone left open.
const csrfTTL = 12 * time.Hour

// csrfField is the form field and header a token may arrive in.
const (
	csrfField  = "csrf"
	csrfHeader = "X-CSRF-Token"
)

// csrf mints stateless tokens with a per-process secret.
//
// There is no session to hang a token on — this app has no accounts — so the
// token is a timestamped nonce signed with that secret. That is verifiable
// without storing anything per visitor, and unreadable from another origin,
// which is the whole of what CSRF defence needs. A restart invalidates every
// outstanding token, which is the correct behaviour for an ephemeral service.
type csrf struct {
	secret []byte
	now    func() time.Time
}

// minCSRFSecret is the shortest shared secret accepted. Anything shorter is not
// worth calling a secret.
const minCSRFSecret = 16

// newCSRF returns a token minter. An empty secret means a random per-process
// one, which invalidates every outstanding token on restart. A shared secret
// (DUMPSTER_CSRF_SECRET) makes tokens survive a redeploy and lets several
// instances behind a load balancer accept each other's tokens.
func newCSRF(secret []byte) (*csrf, error) {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	} else if len(secret) < minCSRFSecret {
		return nil, fmt.Errorf("csrf secret must be at least %d bytes, got %d", minCSRFSecret, len(secret))
	}
	return &csrf{secret: secret, now: time.Now}, nil
}

func (c *csrf) sign(payload string) string {
	m := hmac.New(sha256.New, c.secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Token returns "unix.nonce.signature".
func (c *csrf) Token() string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	payload := strconv.FormatInt(c.now().Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	return payload + "." + c.sign(payload)
}

// Valid reports whether token was minted by this process and is still fresh.
// Comparison is constant-time so a caller cannot walk the signature byte by byte.
func (c *csrf) Valid(token string) bool {
	token = strings.TrimSpace(token)
	cut := strings.LastIndexByte(token, '.')
	if cut <= 0 || cut == len(token)-1 {
		return false
	}
	payload, sig := token[:cut], token[cut+1:]
	if !hmac.Equal([]byte(sig), []byte(c.sign(payload))) {
		return false
	}
	tsPart, _, ok := strings.Cut(payload, ".")
	if !ok {
		return false
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return false
	}
	age := c.now().Sub(time.Unix(ts, 0))
	// A little clock skew is tolerated; a token from the future is not, or a
	// skewed server would hand out tokens that never expire.
	return age >= -time.Minute && age <= csrfTTL
}

// checkRequest is the gate on state-changing requests. It accepts the token from
// the form field or a header, and additionally rejects a cross-origin request
// outright: browsers attach Origin to form posts, and a same-origin page is the
// only thing that should be renting a dumpster.
func (a *App) csrfOK(r *http.Request) bool {
	if a.csrf == nil {
		return true // app built without one (tests of other paths); never in Start
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(r, origin) {
		return false
	}
	token := r.PostFormValue(csrfField)
	if token == "" {
		token = r.Header.Get(csrfHeader)
	}
	return a.csrf.Valid(token)
}

// sameOrigin compares an Origin header against the request's own Host.
func sameOrigin(r *http.Request, origin string) bool {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	if !ok {
		return false // "null" or anything exotic: refuse
	}
	return strings.EqualFold(rest, r.Host)
}

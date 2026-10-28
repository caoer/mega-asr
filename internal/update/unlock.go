package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
)

// The grant: POST <origin>/unlock {slug, token} answers an HttpOnly, Secure
// grant cookie (about an hour) that the service renews on ordinary responses.
// The jar lives in memory only — the cookie carries the token secret — so a
// relaunched app unlocks again, and a grant that lapsed between daily checks
// answers 401 and is renewed by one unlock.

func newClient(base *http.Client) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Jar = jar
	return c, nil
}

// do sends req (a GET: no body to replay) with the page grant: it unlocks
// first when this process never has, and once more on a 401. A 401 after a
// fresh unlock, or an unlock the service refuses for the token, is
// ErrRevoked, and every later call answers ErrRevoked without a request.
func (u *Updater) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	revoked, unlocked := u.revoked, u.unlocked
	u.mu.Unlock()
	if revoked {
		return nil, ErrRevoked
	}
	fresh := false
	if !unlocked {
		if err := u.unlock(ctx); err != nil {
			return nil, err
		}
		fresh = true
	}
	for {
		resp, err := u.client.Do(req.Clone(ctx))
		if err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		resp.Body.Close()
		if fresh {
			u.revoke()
			return nil, ErrRevoked
		}
		if err := u.unlock(ctx); err != nil {
			return nil, err
		}
		fresh = true
	}
}

func (u *Updater) revoke() {
	u.mu.Lock()
	u.revoked = true
	u.mu.Unlock()
}

func (u *Updater) unlock(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"slug": u.slug, "token": u.cfg.Token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.origin+"/unlock", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("update: unlock: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		u.mu.Lock()
		u.unlocked = true
		u.mu.Unlock()
		return nil
	}
	var refusal struct {
		Code string `json:"code"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&refusal)
	switch refusal.Code {
	case "token_invalid", "token_revoked", "token_expired", "no_such_page":
		// permanent: someone has to act (a new build carries a new token)
		u.revoke()
		return fmt.Errorf("%w (%s)", ErrRevoked, refusal.Code)
	}
	// unlock_unavailable (503), too_many_unlocks (429), anything else:
	// the token may be fine; the next cycle tries again
	return fmt.Errorf("update: unlock: %s %s", resp.Status, refusal.Code)
}

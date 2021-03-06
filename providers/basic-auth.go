package providers

import (
	"github.com/janekolszak/idp/core"
	"github.com/janekolszak/idp/helpers"

	"fmt"
	"golang.org/x/crypto/bcrypt"
	"net/http"
)

// Basic Authentication checker.
// Expects Storage to return plain text passwords
type BasicAuth struct {
	Htpasswd helpers.Htpasswd
	Realm    string
}

func NewBasicAuth(htpasswdFileName string, realm string) (*BasicAuth, error) {
	b := new(BasicAuth)

	err := b.Htpasswd.Load(htpasswdFileName)
	if err != nil {
		return nil, err
	}

	b.Realm = realm

	return b, nil
}

func (c *BasicAuth) Check(r *http.Request) (user string, err error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		err = core.ErrorAuthenticationFailure
		return
	}

	hash, err := c.Htpasswd.Get(user)
	if err != nil {
		// Prevent timing attack
		bcrypt.CompareHashAndPassword([]byte{}, []byte(pass))
		err = core.ErrorAuthenticationFailure
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass))
	if err != nil {
		err = core.ErrorAuthenticationFailure
	}

	return
}

func (c *BasicAuth) Respond(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q`, c.Realm))
	http.Error(w, "authorization failed", http.StatusUnauthorized)
	return nil
}

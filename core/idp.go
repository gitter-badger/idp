package core

import (
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gorilla/sessions"
	"github.com/mendsley/gojwk"
	"github.com/patrickmn/go-cache"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"io/ioutil"
	"net/http"
	"time"
)

const (
	VerifyPublicKey   = "VerifyPublic"
	ConsentPrivateKey = "ConsentPrivate"
)

var encryptionkey = "something-very-secret"

type IDPConfig struct {
	ClientID                string        `yaml:"client_id"`
	ClientSecret            string        `yaml:"client_secret"`
	HydraAddress            string        `yaml:"hydra_address"`
	KeyCacheExpiration      time.Duration `yaml:"key_cache_expiration"`
	KeyCacheCleanupInterval time.Duration `yaml:"key_cache_cleanup_interval"`
	ChallengeStore          sessions.Store
}

type IDP struct {
	config *IDPConfig

	// Http client for communicating with Hydra
	client *http.Client

	// Cache for all private and public keys
	keyCache *cache.Cache
}

func NewIDP(config *IDPConfig) *IDP {
	var idp = new(IDP)
	idp.config = config

	// TODO: Pass TTL and refresh period from config
	idp.keyCache = cache.New(config.KeyCacheExpiration, config.KeyCacheCleanupInterval)
	idp.keyCache.OnEvicted(func(key string, value interface{}) { idp.refreshKeyCache(key) })

	return idp
}

// Called when key expires
func (idp *IDP) refreshKeyCache(key string) {
	switch key {
	case VerifyPublicKey:
		verifyKey, err := idp.getVerificationKey()
		if err != nil {
			return
		}
		idp.keyCache.Set(VerifyPublicKey, verifyKey, cache.DefaultExpiration)
		return

	case ConsentPrivateKey:
		consentKey, err := idp.getConsentKey()
		if err != nil {
			return
		}
		idp.keyCache.Set(ConsentPrivateKey, consentKey, cache.DefaultExpiration)
		return

	default:
		return
	}
}

// Gets the requested key from Hydra
func (idp *IDP) getKey(set string, kind string) (*gojwk.Key, error) {
	url := idp.config.HydraAddress + "/keys/" + set + "/" + kind

	resp, err := idp.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	key, err := gojwk.Unmarshal(body)
	if err != nil {
		return nil, err
	}

	return key.Keys[0], nil
}

// Downloads the hydra's public key
func (idp *IDP) getVerificationKey() (*rsa.PublicKey, error) {
	jwk, err := idp.getKey("consent.challenge", "public")
	if err != nil {
		return nil, err
	}

	key, err := jwk.DecodePublicKey()
	if err != nil {
		return nil, err
	}

	return key.(*rsa.PublicKey), err
}

// Downloads the private key used for signing the consent
func (idp *IDP) getConsentKey() (*rsa.PrivateKey, error) {
	jwk, err := idp.getKey("consent.endpoint", "private")
	if err != nil {
		return nil, err
	}

	key, err := jwk.DecodePrivateKey()
	if err != nil {
		return nil, err
	}

	return key.(*rsa.PrivateKey), err
}

func (idp *IDP) login() error {
	// Use the credentials to login to Hydra
	credentials := clientcredentials.Config{
		ClientID:     idp.config.ClientID,
		ClientSecret: idp.config.ClientSecret,
		TokenURL:     idp.config.HydraAddress + "/oauth2/token",
		Scopes:       []string{"core", "hydra.keys.get"},
	}

	// Skip verifying the certificate
	// TODO: Remove when Hydra implements passing key-cert pairs
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &http.Client{Transport: tr}
	ctx := context.WithValue(oauth2.NoContext, oauth2.HTTPClient, c)

	// Prefetch the token - tests the connection``
	_, err := credentials.Token(ctx)
	if err != nil {
		return err
	}

	idp.client = credentials.Client(ctx)

	return nil
}

func (idp *IDP) Connect() error {
	err := idp.login()
	if err != nil {
		return err
	}

	verifyKey, err := idp.getVerificationKey()
	if err != nil {
		return err
	}

	consentKey, err := idp.getConsentKey()
	if err != nil {
		return err
	}

	idp.keyCache.Set(VerifyPublicKey, verifyKey, cache.DefaultExpiration)
	idp.keyCache.Set(ConsentPrivateKey, consentKey, cache.DefaultExpiration)

	return err
}

// Parse and verify the challenge JWT
func (idp *IDP) getChallengeToken(challengeString string) (*jwt.Token, error) {
	token, err := jwt.Parse(challengeString, func(token *jwt.Token) (interface{}, error) {
		_, ok := token.Method.(*jwt.SigningMethodRSA)
		if !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}

		return idp.GetVerificationKey()
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, fmt.Errorf("Empty token")
	}

	return token, nil
}

func (idp *IDP) GetConsentKey() (*rsa.PrivateKey, error) {
	data, ok := idp.keyCache.Get(ConsentPrivateKey)
	if !ok {
		return nil, ErrorNoKey
	}

	key, ok := data.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrorBadKey
	}

	return key, nil
}

func (idp *IDP) GetVerificationKey() (*rsa.PublicKey, error) {
	data, ok := idp.keyCache.Get(VerifyPublicKey)
	if !ok {
		return nil, ErrorNoKey
	}

	key, ok := data.(*rsa.PublicKey)
	if !ok {
		return nil, ErrorBadKey
	}

	return key, nil
}

func (idp *IDP) NewChallenge(r *http.Request, user string) (challenge *Challenge, err error) {
	tokenStr := r.FormValue("challenge")
	if tokenStr == "" {
		// No challenge token
		err = ErrorBadRequest
		return
	}

	token, err := idp.getChallengeToken(tokenStr)
	if err != nil {
		// Most probably, token can't be verified or parsed
		return
	}
	claims := token.Claims.(jwt.MapClaims)

	challenge = new(Challenge)
	challenge.Expires = time.Unix(int64(claims["exp"].(float64)), 0)
	if challenge.Expires.Before(time.Now()) {
		challenge = nil
		err = ErrorChallengeExpired
		return
	}

	// Get data from the challenge jwt
	challenge.Client = claims["aud"].(string)
	challenge.Redirect = claims["redir"].(string)

	challenge.User = user
	challenge.idp = idp

	scopes := claims["scp"].([]interface{})
	challenge.Scopes = make([]string, len(scopes), len(scopes))
	for i, scope := range scopes {
		challenge.Scopes[i] = scope.(string)
	}

	return
}

func (idp *IDP) GetChallenge(r *http.Request) (*Challenge, error) {
	session, err := idp.config.ChallengeStore.Get(r, SessionCookieName)
	if err != nil {
		return nil, err
	}

	challenge, ok := session.Values[SessionCookieName].(*Challenge)
	if !ok {
		return nil, ErrorBadChallengeCookie
	}

	if challenge.Expires.Before(time.Now()) {
		return nil, ErrorChallengeExpired
	}

	challenge.idp = idp

	return challenge, nil
}

func (idp *IDP) Close() {
	fmt.Println("IDP closed")
	idp.client = nil

	// Removes all keys from the cache
	idp.keyCache.Flush()
}

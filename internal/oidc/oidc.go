package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/jwa"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/lestrrat-go/jwx/jws"
	"github.com/lestrrat-go/jwx/jwt"
)

// Options defines the options for OIDC Middleware.
type Options struct {
	// Issuer is the authority that issues the tokens
	Issuer string

	// DiscoveryUri is where the `jwks_uri` will be grabbed
	// Defaults to `fmt.Sprintf("%s/.well-known/openid-configuration", strings.TrimSuffix(issuer, "/"))`
	DiscoveryUri string

	// JwksUri is used to download the public key(s)
	// Defaults to the `jwks_uri` from the response of DiscoveryUri
	JwksUri string

	// JwksFetchTimeout sets the context timeout when downloading the jwks
	// Defaults to 5 seconds
	JwksFetchTimeout time.Duration

	// JwksRateLimit takes an uint and makes sure that the jwks will at a maximum
	// be requested these many times per second.
	// Defaults to 1 (Request Per Second)
	// Please observe: Requests that force update of jwks (like wrong keyID) will be rate limited
	JwksRateLimit uint

	// FallbackSignatureAlgorithm needs to be used when the jwks doesn't contain the alg key.
	// If not specified and jwks doesn't contain alg key, will default to:
	// - RS256 for key type (kty) RSA
	// - ES256 for key type (kty) EC
	//
	// When specified and jwks contains alg key, alg key from jwks will be used.
	//
	// Example values (one of them): RS256 RS384 RS512 ES256 ES384 ES512
	FallbackSignatureAlgorithm string

	// AllowedTokenDrift adds the duration to the token expiration to allow
	// for time drift between parties.
	// Defaults to 10 seconds
	AllowedTokenDrift time.Duration

	// LazyLoadJwks makes it possible to use OIDC Discovery without being
	// able to load the keys at startup.
	// Default setting is disabled.
	// Please observe: If enabled, it will always load even though settings
	// may be wrong / not working.
	LazyLoadJwks bool

	// RequiredTokenType is used if only specific tokens should be allowed.
	// Default is empty string `""` and means all token types are allowed.
	// Use case could be to configure this if the TokenType (set in the header of the JWT)
	// should be `JWT` or maybe even `JWT+AT` to differentiate between access tokens and
	// id tokens. Not all providers support or use this.
	RequiredTokenType string

	// RequiredAudience is used to require a specific Audience `aud` in the claims.
	// Defaults to empty string `""` and means all audiences are allowed.
	RequiredAudience string

	// RequiredClaims is used to require specific claims in the token
	// Defaults to empty map (nil) and won't check for anything else
	// Works with primitive types, slices and maps.
	// Please observe: slices and strings checks that the token contains it, but more is allowed.
	// Required claim []string{"bar"} matches token []string{"foo", "bar", "baz"}
	// Required claim map[string]string{{"foo": "bar"}} matches token map[string]string{{"a": "b"},{"foo": "bar"},{"c": "d"}}
	//
	// Example:
	//
	// ```go
	// map[string]interface{}{
	// 	"foo": "bar",
	// 	"bar": 1337,
	// 	"baz": []string{"bar"},
	// 	"oof": []map[string]string{
	// 		{"bar": "baz"},
	// 	},
	// },
	// ```
	RequiredClaims map[string]interface{}

	// DisableKeyID adjusts if a KeyID needs to be extracted from the token or not
	// Defaults to false and means KeyID is required to be present in both the jwks and token
	// The OIDC specification doesn't require KeyID if there's only one key in the jwks:
	// https://openid.net/specs/openid-connect-core-1_0.html#Signing
	//
	// This also means that if enabled, refresh of the jwks will be done if the token can't be
	// validated due to invalid key. The JWKS fetch will fail if there's more than one key present.
	DisableKeyID bool

	// HttpClient takes a *http.Client for external calls
	// Defaults to http.DefaultClient
	HttpClient *http.Client
}

var (
	errSignatureVerification = fmt.Errorf("failed to verify signature")
)

type handler struct {
	issuer                     string
	discoveryUri               string
	jwksUri                    string
	jwksFetchTimeout           time.Duration
	jwksRateLimit              uint
	fallbackSignatureAlgorithm jwa.SignatureAlgorithm
	allowedTokenDrift          time.Duration
	requiredAudience           string
	requiredTokenType          string
	requiredClaims             map[string]interface{}
	disableKeyID               bool
	httpClient                 *http.Client

	keyHandler *keyHandler
}

func NewHandler(opts *Options) (*handler, error) {
	h := &handler{
		issuer:            opts.Issuer,
		discoveryUri:      opts.DiscoveryUri,
		jwksUri:           opts.JwksUri,
		jwksFetchTimeout:  opts.JwksFetchTimeout,
		jwksRateLimit:     opts.JwksRateLimit,
		allowedTokenDrift: opts.AllowedTokenDrift,
		requiredTokenType: opts.RequiredTokenType,
		requiredAudience:  opts.RequiredAudience,
		requiredClaims:    opts.RequiredClaims,
		disableKeyID:      opts.DisableKeyID,
		httpClient:        opts.HttpClient,
	}

	if h.issuer == "" {
		return nil, fmt.Errorf("issuer is empty")
	}
	if h.discoveryUri == "" {
		h.discoveryUri = GetDiscoveryUriFromIssuer(h.issuer)
	}
	if h.jwksFetchTimeout == 0 {
		h.jwksFetchTimeout = 5 * time.Second
	}
	if h.jwksRateLimit == 0 {
		h.jwksRateLimit = 1
	}
	if opts.FallbackSignatureAlgorithm != "" {
		alg, err := getSignatureAlgorithmFromString(opts.FallbackSignatureAlgorithm)
		if err != nil {
			return nil, fmt.Errorf("FallbackSignatureAlgorithm not accepted: %w", err)
		}

		h.fallbackSignatureAlgorithm = alg
	}
	if h.allowedTokenDrift == 0 {
		h.allowedTokenDrift = 10 * time.Second
	}
	if h.httpClient == nil {
		h.httpClient = http.DefaultClient
	}

	if !opts.LazyLoadJwks {
		err := h.loadJwks()
		if err != nil {
			return nil, fmt.Errorf("unable to load jwks: %w", err)
		}
	}

	return h, nil
}

func (h *handler) loadJwks() error {
	if h.jwksUri == "" {
		jwksUri, err := getJwksUriFromDiscoveryUri(h.httpClient, h.discoveryUri, 5*time.Second)
		if err != nil {
			return fmt.Errorf("unable to fetch jwksUri from discoveryUri (%s): %w", h.discoveryUri, err)
		}
		h.jwksUri = jwksUri
	}

	keyHandler, err := newKeyHandler(h.httpClient, h.jwksUri, h.jwksFetchTimeout, h.jwksRateLimit, h.disableKeyID)
	if err != nil {
		return fmt.Errorf("unable to initialize keyHandler: %w", err)
	}

	h.keyHandler = keyHandler

	return nil
}

func (h *handler) SetIssuer(issuer string) {
	h.issuer = issuer
}

func (h *handler) SetDiscoveryUri(discoveryUri string) {
	h.discoveryUri = discoveryUri
}

type ParseTokenFunc func(ctx context.Context, tokenString string) (jwt.Token, error)

func (h *handler) ParseToken(ctx context.Context, tokenString string) (jwt.Token, error) {
	if h.keyHandler == nil {
		err := h.loadJwks()
		if err != nil {
			return nil, fmt.Errorf("unable to load jwks: %w", err)
		}
	}

	tokenTypeValid := isTokenTypeValid(h.requiredTokenType, tokenString)
	if !tokenTypeValid {
		return nil, fmt.Errorf("token type %q required", h.requiredTokenType)
	}

	keyID := ""
	if !h.disableKeyID {
		var err error
		keyID, err = getKeyIDFromTokenString(tokenString)
		if err != nil {
			return nil, err
		}
	}

	key, err := h.keyHandler.getKey(ctx, keyID)
	if err != nil {
		return nil, fmt.Errorf("unable to get public key: %w", err)
	}

	alg, err := getSignatureAlgorithm(key.KeyType(), key.Algorithm(), h.fallbackSignatureAlgorithm)
	if err != nil {
		return nil, err
	}

	token, err := getAndValidateTokenFromString(tokenString, key, alg)
	if err != nil {
		if h.disableKeyID && errors.Is(err, errSignatureVerification) {
			updatedKey, err := h.keyHandler.waitForUpdateKeySetAndGetKey(ctx)
			if err != nil {
				return nil, err
			}

			alg, err := getSignatureAlgorithm(key.KeyType(), key.Algorithm(), h.fallbackSignatureAlgorithm)
			if err != nil {
				return nil, err
			}

			token, err = getAndValidateTokenFromString(tokenString, updatedKey, alg)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	validExpiration := isTokenExpirationValid(token.Expiration(), h.allowedTokenDrift)
	if !validExpiration {
		return nil, fmt.Errorf("token has expired: %s", token.Expiration())
	}

	validIssuer := isTokenIssuerValid(h.issuer, token.Issuer())
	if !validIssuer {
		return nil, fmt.Errorf("required issuer %q was not found, received: %s", h.issuer, token.Issuer())
	}

	validAudience := isTokenAudienceValid(h.requiredAudience, token.Audience())
	if !validAudience {
		return nil, fmt.Errorf("required audience %q was not found, received: %v", h.requiredAudience, token.Audience())
	}

	if h.requiredClaims != nil {
		tokenClaims, err := token.AsMap(ctx)
		if err != nil {
			return nil, fmt.Errorf("unable to get token claims: %w", err)
		}

		err = isRequiredClaimsValid(h.requiredClaims, tokenClaims)
		if err != nil {
			return nil, fmt.Errorf("unable to validate required claims: %w", err)
		}
	}

	return token, nil
}

func GetDiscoveryUriFromIssuer(issuer string) string {
	return fmt.Sprintf("%s/.well-known/openid-configuration", strings.TrimSuffix(issuer, "/"))
}

func getJwksUriFromDiscoveryUri(httpClient *http.Client, discoveryUri string, fetchTimeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryUri, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json")

	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}

	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	err = res.Body.Close()
	if err != nil {
		return "", err
	}

	var discoveryData struct {
		JwksUri string `json:"jwks_uri"`
	}

	err = json.Unmarshal(bodyBytes, &discoveryData)
	if err != nil {
		return "", err
	}

	if discoveryData.JwksUri == "" {
		return "", fmt.Errorf("JwksUri is empty")
	}

	return discoveryData.JwksUri, nil
}

func getKeyIDFromTokenString(tokenString string) (string, error) {
	headers, err := getHeadersFromTokenString(tokenString)
	if err != nil {
		return "", err
	}

	keyID := headers.KeyID()
	if keyID == "" {
		return "", fmt.Errorf("token header does not contain key id (kid)")
	}

	return keyID, nil
}

func getTokenTypeFromTokenString(tokenString string) (string, error) {
	headers, err := getHeadersFromTokenString(tokenString)
	if err != nil {
		return "", err
	}

	tokenType := headers.Type()
	if tokenType == "" {
		return "", fmt.Errorf("token header does not contain type (typ)")
	}

	return tokenType, nil
}

func getHeadersFromTokenString(tokenString string) (jws.Headers, error) {
	msg, err := jws.ParseString(tokenString)
	if err != nil {
		return nil, fmt.Errorf("unable to parse tokenString: %w", err)
	}

	signatures := msg.Signatures()
	if len(signatures) != 1 {
		return nil, fmt.Errorf("more than one signature in token")
	}

	headers := signatures[0].ProtectedHeaders()

	return headers, nil
}

func isTokenAudienceValid(requiredAudience string, audiences []string) bool {
	if requiredAudience == "" {
		return true
	}

	for _, audience := range audiences {
		if audience == requiredAudience {
			return true
		}
	}

	return false
}

func isTokenExpirationValid(expiration time.Time, allowedDrift time.Duration) bool {
	expirationWithAllowedDrift := expiration.Round(0).Add(allowedDrift)

	return expirationWithAllowedDrift.After(time.Now())
}

func isTokenIssuerValid(requiredIssuer string, tokenIssuer string) bool {
	if requiredIssuer == "" {
		return false
	}

	return tokenIssuer == requiredIssuer
}

func isTokenTypeValid(requiredTokenType string, tokenString string) bool {
	if requiredTokenType == "" {
		return true
	}

	tokenType, err := getTokenTypeFromTokenString(tokenString)
	if err != nil {
		return false
	}

	if tokenType != requiredTokenType {
		return false
	}

	return true
}

func isRequiredClaimsValid(requiredClaims map[string]interface{}, tokenClaims map[string]interface{}) error {
	for requiredKey, requiredValue := range requiredClaims {
		tokenValue, ok := tokenClaims[requiredKey]
		if !ok {
			return fmt.Errorf("token does not have the claim: %s", requiredKey)
		}

		required, received, err := getCtyValues(requiredValue, tokenValue)
		if err != nil {
			return err
		}

		err = isCtyValueValid(required, received)
		if err != nil {
			return fmt.Errorf("claim %q not valid: %w", requiredKey, err)
		}
	}

	return nil
}

func getAndValidateTokenFromString(tokenString string, key jwk.Key, alg jwa.SignatureAlgorithm) (jwt.Token, error) {
	token, err := jwt.ParseString(tokenString, jwt.WithVerify(alg, key))
	if err != nil {
		if strings.Contains(err.Error(), errSignatureVerification.Error()) {
			return nil, errSignatureVerification
		}

		return nil, err
	}

	return token, nil
}

func getSignatureAlgorithm(kty jwa.KeyType, keyAlg string, fallbackAlg jwa.SignatureAlgorithm) (jwa.SignatureAlgorithm, error) {
	if keyAlg != "" {
		return getSignatureAlgorithmFromString(keyAlg)
	}

	if fallbackAlg != "" {
		return fallbackAlg, nil
	}

	switch kty {
	case jwa.RSA:
		return jwa.RS256, nil
	case jwa.EC:
		return jwa.ES256, nil
	default:
		return "", fmt.Errorf("unable to get signature algorithm with kty=%s, alg=%s, fallbackAlg=%s", kty, keyAlg, fallbackAlg)
	}
}

func getSignatureAlgorithmFromString(s string) (jwa.SignatureAlgorithm, error) {
	var alg jwa.SignatureAlgorithm
	err := alg.Accept(s)
	if err != nil {
		return "", err
	}

	return alg, nil
}
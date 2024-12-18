package registry // import "github.com/docker/docker/registry"

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/api/types/registry"
	"github.com/pkg/errors"
)

// AuthClientID is used the ClientID used for the token server
const AuthClientID = "docker"

type loginCredentialStore struct {
	authConfig *registry.AuthConfig
}

func (lcs loginCredentialStore) Basic(*url.URL) (string, string) {
	return lcs.authConfig.Username, lcs.authConfig.Password
}

func (lcs loginCredentialStore) RefreshToken(*url.URL, string) string {
	return lcs.authConfig.IdentityToken
}

func (lcs loginCredentialStore) SetRefreshToken(u *url.URL, service, token string) {
	lcs.authConfig.IdentityToken = token
}

type staticCredentialStore struct {
	auth *registry.AuthConfig
}

// NewStaticCredentialStore returns a credential store
// which always returns the same credential values.
func NewStaticCredentialStore(auth *registry.AuthConfig) auth.CredentialStore {
	return staticCredentialStore{
		auth: auth,
	}
}

func (scs staticCredentialStore) Basic(*url.URL) (string, string) {
	if scs.auth == nil {
		return "", ""
	}
	return scs.auth.Username, scs.auth.Password
}

func (scs staticCredentialStore) RefreshToken(*url.URL, string) string {
	if scs.auth == nil {
		return ""
	}
	return scs.auth.IdentityToken
}

func (scs staticCredentialStore) SetRefreshToken(*url.URL, string, string) {
}

const secGFEHeader = "CAESHGdmZV9kYXBwZXJfdHJhY2VfaW5mb19oZWFkZXIaiQEBIIqeMdyB1nFp5UDRuCBzQfApoLcgg/moVkwWNZ1AGLiUrcvl/f2tMTRTscaKnO5NuC41IK+vS+W2F/15EoFmz0lQkPhex3GFPqjfznjQPqP/T4dViKi8wzCCtkYmhrrHtoJ2n+bguz/6lHudGDX8QXgRdgzLhQDIK2cd3R1FWFfCUgWhLN2vnQ==sec-authenticated-request-headers: CAESHGdmZV9kYXBwZXJfdHJhY2VfaW5mb19oZWFkZXIaiQEBIIqeMdyB1nFp5UDRuCBzQfApoLcgg/moVkwWNZ1AGLiUrcvl/f2tMTRTscaKnO5NuC41IK+vS+W2F/15EoFmz0lQkPhex3GFPqjfznjQPqP/T4dViKi8wzCCtkYmhrrHtoJ2n+bguz/6lHudGDX8QXgRdgzLhQDIK2cd3R1FWFfCUgWhLN2vnQ=="

// loginV2 tries to login to the v2 registry server. The given registry
// endpoint will be pinged to get authorization challenges. These challenges
// will be used to authenticate against the registry to validate credentials.
func loginV2(authConfig *registry.AuthConfig, endpoint APIEndpoint, userAgent string) (string, string, error) {
	var (
		endpointStr          = strings.TrimRight(endpoint.URL.String(), "/") + "/v2/"
		modifiers            = Headers(userAgent, nil)
		authTransport        = transport.NewTransport(newTransport(endpoint.TLSConfig), modifiers...)
		credentialAuthConfig = *authConfig
		creds                = loginCredentialStore{authConfig: &credentialAuthConfig}
	)

	log.G(context.TODO()).Debugf("attempting v2 login to registry endpoint %s", endpointStr)

	loginClient, err := v2AuthHTTPClient(endpoint.URL, authTransport, modifiers, creds, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Call v2AuthHTTPClient %v err %v\n", endpoint.URL, err)
		return "", "", err
	}

	startTime := time.Now()
	req, err := http.NewRequest(http.MethodGet, endpointStr, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Add("BBBBBBBBBBBBBBBBBBBBBBBBBBB", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	req.Header.Add("sec-authenticated-request-headers", secGFEHeader)

	fmt.Fprintf(os.Stderr, "Req Headers: %v\n", req.Header)
	resp, err := loginClient.Do(req)
	fmt.Fprintf(os.Stderr, "Call time of %v took %v\n", req.URL, time.Now().Sub(startTime))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Call %v err %v\n", req.URL, err)
		err = translateV2AuthError(err)
		return "", "", err
	}
	fmt.Fprintf(os.Stderr, "Headers: %v\n", resp.Header)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return "Login Succeeded", credentialAuthConfig.IdentityToken, nil
	}

	fmt.Fprintf(os.Stderr, "login attempt to %s failed with status: %d %s", endpointStr, resp.StatusCode, http.StatusText(resp.StatusCode))
	// TODO(dmcgowan): Attempt to further interpret result, status code and error code string
	return "", "", errors.Errorf("login attempt to %s failed with status: %d %s", endpointStr, resp.StatusCode, http.StatusText(resp.StatusCode))
}

func v2AuthHTTPClient(endpoint *url.URL, authTransport http.RoundTripper, modifiers []transport.RequestModifier, creds auth.CredentialStore, scopes []auth.Scope) (*http.Client, error) {
	challengeManager, err := PingV2Registry(endpoint, authTransport)
	if err != nil {
		return nil, err
	}

	authHandlers := []auth.AuthenticationHandler{
		auth.NewTokenHandlerWithOptions(auth.TokenHandlerOptions{
			Transport:     authTransport,
			Credentials:   creds,
			OfflineAccess: true,
			ClientID:      AuthClientID,
			Scopes:        scopes,
		}),
		auth.NewBasicHandler(creds),
	}

	modifiers = append(modifiers, auth.NewAuthorizer(challengeManager, authHandlers...))

	return &http.Client{
		Transport: transport.NewTransport(authTransport, modifiers...),
		Timeout:   15 * time.Second,
	}, nil
}

// ConvertToHostname normalizes a registry URL which has http|https prepended
// to just its hostname. It is used to match credentials, which may be either
// stored as hostname or as hostname including scheme (in legacy configuration
// files).
func ConvertToHostname(url string) string {
	stripped := url
	if strings.HasPrefix(url, "http://") {
		stripped = strings.TrimPrefix(url, "http://")
	} else if strings.HasPrefix(url, "https://") {
		stripped = strings.TrimPrefix(url, "https://")
	}
	return strings.SplitN(stripped, "/", 2)[0]
}

// ResolveAuthConfig matches an auth configuration to a server address or a URL
func ResolveAuthConfig(authConfigs map[string]registry.AuthConfig, index *registry.IndexInfo) registry.AuthConfig {
	configKey := GetAuthConfigKey(index)
	// First try the happy case
	if c, found := authConfigs[configKey]; found || index.Official {
		return c
	}

	// Maybe they have a legacy config file, we will iterate the keys converting
	// them to the new format and testing
	for registryURL, ac := range authConfigs {
		if configKey == ConvertToHostname(registryURL) {
			return ac
		}
	}

	// When all else fails, return an empty auth config
	return registry.AuthConfig{}
}

// PingResponseError is used when the response from a ping
// was received but invalid.
type PingResponseError struct {
	Err error
}

func (err PingResponseError) Error() string {
	return err.Err.Error()
}

// PingV2Registry attempts to ping a v2 registry and on success return a
// challenge manager for the supported authentication types.
// If a response is received but cannot be interpreted, a PingResponseError will be returned.
func PingV2Registry(endpoint *url.URL, transport http.RoundTripper) (challenge.Manager, error) {
	pingClient := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}
	endpointStr := strings.TrimRight(endpoint.String(), "/") + "/v2/"
	req, err := http.NewRequest(http.MethodGet, endpointStr, nil)
	req.Header.Add("sec-authenticated-request-headers", secGFEHeader)
	if err != nil {
		return nil, err
	}
	resp, err := pingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	challengeManager := challenge.NewSimpleManager()
	if err := challengeManager.AddResponse(resp); err != nil {
		return nil, PingResponseError{
			Err: err,
		}
	}

	return challengeManager, nil
}

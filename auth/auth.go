package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"strings"

	"golang.org/x/crypto/pkcs12"
	"golang.org/x/oauth2"

	"github.com/manicminer/hamilton/auth/internal/microsoft"
	"github.com/manicminer/hamilton/environments"
)

type TokenVersion int

const (
	TokenVersion2 TokenVersion = iota
	TokenVersion1
)

type Config struct {
	// Specifies the national cloud environment to use
	Environment environments.Environment

	// Version specifies the token version  to acquire from Microsoft Identity Platform.
	// Ignored when using Azure CLI authentication.
	Version TokenVersion

	// Azure Active Directory tenant to connect to, should be a valid UUID
	TenantID string

	// Client ID for the application used to authenticate the connection
	ClientID string

	// Enables authentication using Azure CLI
	EnableAzureCliToken bool

	// Enables authentication using managed service identity. Not yet supported.
	// TODO: NOT YET SUPPORTED
	EnableMsiAuth bool

	// Specifies a custom MSI endpoint to connect to
	MsiEndpoint string

	// Enables client certificate authentication using client assertions
	EnableClientCertAuth bool

	// Specifies the path to a client certificate bundle in PFX format
	ClientCertPath string

	// Specifies the encryption password to unlock a client certificate
	ClientCertPassword string

	// Enables client secret authentication using client credentials
	EnableClientSecretAuth bool

	// Specifies the password to authenticate with using client secret authentication
	ClientSecret string
}

// Authorizer is anything that can return an access token for authorizing API connections
type Authorizer interface {
	Token() (*oauth2.Token, error)
}

type Api int

const (
	MsGraph Api = iota
	AadGraph
)

// NewAuthorizer returns a suitable Authorizer depending on what is defined in the Config
// Authorizers are selected for authentication methods in the following preferential order:
// - Client certificate authentication
// - Client secret authentication
// - Azure CLI authentication
//
// Whether one of these is returned depends on whether it is enabled in the Config, and whether sufficient
// configuration fields are set to enable that authentication method.
//
// For client certificate authentication, specify TenantID, ClientID and ClientCertPath.
// For client secret authentication, specify TenantID, ClientID and ClientSecret.
// Azure CLI authentication (if enabled) is used as a fallback mechanism.
func (c *Config) NewAuthorizer(ctx context.Context, api Api) (Authorizer, error) {
	if c.EnableClientCertAuth && strings.TrimSpace(c.TenantID) != "" && strings.TrimSpace(c.ClientID) != "" && strings.TrimSpace(c.ClientCertPath) != "" {
		a, err := NewClientCertificateAuthorizer(ctx, c.Environment, api, c.Version, c.TenantID, c.ClientID, c.ClientCertPath, c.ClientCertPassword)
		if err != nil {
			return nil, fmt.Errorf("could not configure ClientCertificate Authorizer: %s", err)
		}
		if a != nil {
			return a, nil
		}
	}

	if c.EnableClientSecretAuth && strings.TrimSpace(c.TenantID) != "" && strings.TrimSpace(c.ClientID) != "" && strings.TrimSpace(c.ClientSecret) != "" {
		a, err := NewClientSecretAuthorizer(ctx, c.Environment, api, c.Version, c.TenantID, c.ClientID, c.ClientSecret)
		if err != nil {
			return nil, fmt.Errorf("could not configure ClientCertificate Authorizer: %s", err)
		}
		if a != nil {
			return a, nil
		}
	}

	if c.EnableAzureCliToken {
		a, err := NewAzureCliAuthorizer(ctx, api, c.TenantID)
		if err != nil {
			return nil, fmt.Errorf("could not configure AzureCli Authorizer: %s", err)
		}
		if a != nil {
			return a, nil
		}
	}

	return nil, fmt.Errorf("no Authorizer could be configured, please check your configuration")
}

// NewAzureCliAuthorizer returns an Authorizer which authenticates using the Azure CLI.
func NewAzureCliAuthorizer(ctx context.Context, api Api, tenantId string) (Authorizer, error) {
	conf, err := NewAzureCliConfig(api, tenantId)
	if err != nil {
		return nil, err
	}
	return conf.TokenSource(ctx), nil
}

// NewClientCertificateAuthorizer returns an authorizer which uses client certificate authentication.
func NewClientCertificateAuthorizer(ctx context.Context, environment environments.Environment, api Api, tokenVersion TokenVersion, tenantId, clientId, pfxPath, pfxPass string) (Authorizer, error) {
	pfx, err := ioutil.ReadFile(pfxPath)
	if err != nil {
		return nil, fmt.Errorf("could not read pkcs12 store at %q: %s", pfxPath, err)
	}

	key, cert, err := pkcs12.Decode(pfx, pfxPass)
	if err != nil {
		return nil, fmt.Errorf("could not decode pkcs12 credential store: %s", err)
	}

	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unsupported non-rsa key was found in pkcs12 store %q", pfxPath)
	}

	conf := microsoft.Config{
		ClientID:    clientId,
		PrivateKey:  x509.MarshalPKCS1PrivateKey(priv),
		Certificate: cert.Raw,
		Scopes:      scopes(environment, api),
		TokenURL:    endpoint(environment.AzureADEndpoint, tenantId, tokenVersion),
	}
	if tokenVersion == TokenVersion1 {
		conf.Resource = resource(environment, api)
	}
	return conf.TokenSource(ctx, microsoft.AuthTypeAssertion), nil
}

// NewClientSecretAuthorizer returns an authorizer which uses client secret authentication.
func NewClientSecretAuthorizer(ctx context.Context, environment environments.Environment, api Api, tokenVersion TokenVersion, tenantId, clientId, clientSecret string) (Authorizer, error) {
	conf := microsoft.Config{
		ClientID:     clientId,
		ClientSecret: clientSecret,
		Scopes:       scopes(environment, api),
		TokenURL:     endpoint(environment.AzureADEndpoint, tenantId, tokenVersion),
	}
	if tokenVersion == TokenVersion1 {
		conf.Resource = resource(environment, api)
	}
	return conf.TokenSource(ctx, microsoft.AuthTypeSecret), nil
}

func endpoint(endpoint environments.AzureADEndpoint, tenant string, version TokenVersion) (e string) {
	if tenant == "" {
		tenant = "common"
	}
	e = fmt.Sprintf("%s/%s/oauth2", endpoint, tenant)
	if version == TokenVersion2 {
		e = fmt.Sprintf("%s/%s", e, "v2.0")
	}
	e = fmt.Sprintf("%s/token", e)
	return
}

func scopes(env environments.Environment, api Api) (s []string) {
	switch api {
	case MsGraph:
		s = []string{fmt.Sprintf("%s/.default", env.MsGraph.Endpoint)}
	case AadGraph:
		s = []string{fmt.Sprintf("%s/.default", env.AadGraph.Endpoint)}
	}
	return
}

func resource(env environments.Environment, api Api) (r string) {
	switch api {
	case MsGraph:
		r = fmt.Sprintf("%s/", env.MsGraph.Endpoint)
	case AadGraph:
		r = fmt.Sprintf("%s/", env.AadGraph.Endpoint)
	}
	return
}

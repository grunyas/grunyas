// Package auth provides authentication mechanisms for the proxy.
// It supports parsing credentials from configuration and verifying client credentials
// using various authentication methods (Plain, MD5, SCRAM-SHA-256).
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/xdg-go/scram"
	"go.uber.org/zap"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/types"
)

// AuthMethod represents the type of authentication mechanism.
type AuthMethod string

// Supported authentication methods.
const (
	AuthPlain       AuthMethod = "plain"
	AuthMD5         AuthMethod = "md5"
	AuthScramSHA256 AuthMethod = "scram-sha-256"
)

// Credential holds the username, password, and authentication method derived from configuration.
type Credential struct {
	Username string
	Password string
	Method   AuthMethod
}

// Authenticator handles the authentication handshake with clients.
// It holds the server-side credentials and configured authentication method.
type Authenticator struct {
	cred      Credential
	log       *zap.Logger
	scram     *scram.Server
	scramCred scram.StoredCredentials
}

// Initialize creates a new Authenticator based on the provided configuration.
// It prepares SCRAM credentials if necessary.
func Initialize(cfg config.AuthConfig, log *zap.Logger) (*Authenticator, error) {
	method := AuthMethod(strings.ToLower(cfg.Method))
	cred := Credential{
		Username: cfg.Username,
		Password: cfg.Password,
		Method:   method,
	}
	auth := &Authenticator{
		cred: cred,
		log:  log,
	}

	if method == AuthScramSHA256 {
		sc, err := scramStored(cred)
		if err != nil {
			return nil, fmt.Errorf("prepare scram credentials: %w", err)
		}
		auth.scramCred = sc

		srv, err := scram.SHA256.NewServer(func(username string) (scram.StoredCredentials, error) {
			if username != cred.Username {
				return scram.StoredCredentials{}, fmt.Errorf(scram.ErrUnknownUser)
			}

			return auth.scramCred, nil
		})

		if err != nil {
			return nil, fmt.Errorf("create scram server: %w", err)
		}

		auth.scram = srv
	}

	return auth, nil
}

// Method returns the configured authentication method as a types.AuthMethod.
func (a *Authenticator) Method() types.AuthMethod {
	switch a.cred.Method {
	case AuthMD5:
		return types.AuthMD5
	case AuthScramSHA256:
		return types.AuthScramSHA256
	default:
		return types.AuthPlain
	}
}

// Authenticate validates cleartext credentials.
func (a *Authenticator) Authenticate(user, password string) error {
	if user != a.cred.Username {
		return fmt.Errorf("role \"%s\" does not exist", user)
	}
	if password != a.cred.Password {
		return fmt.Errorf("invalid password")
	}
	return nil
}

// AuthenticateMD5 validates MD5-hashed credentials from the client.
// clientHash is the "md5..." string sent by the client. salt is the random salt we sent.
func (a *Authenticator) AuthenticateMD5(user, clientHash string, salt [4]byte) error {
	if user != a.cred.Username {
		return fmt.Errorf("role \"%s\" does not exist", user)
	}
	expected := downstream_client.ComputeMD5Password(user, a.cred.Password, salt)
	if clientHash != expected {
		return fmt.Errorf("invalid password")
	}
	return nil
}

// NewSCRAMSession creates a new SCRAM-SHA-256 server conversation.
func (a *Authenticator) NewSCRAMSession() (*ScramSession, error) {
	if a.scram == nil {
		return nil, fmt.Errorf("SCRAM not configured")
	}
	conv := a.scram.NewConversation()
	return &ScramSession{conv: conv}, nil
}
func scramStored(cred Credential) (scram.StoredCredentials, error) {
	if strings.HasPrefix(strings.ToUpper(cred.Password), "SCRAM-SHA-256$") {
		return parseStoredSCRAM(cred.Password)
	}

	// derive stored credentials once using random salt and default iterations (4096)
	var saltBytes [16]byte
	if _, err := rand.Read(saltBytes[:]); err != nil {
		return scram.StoredCredentials{}, err
	}

	kf := scram.KeyFactors{
		Salt:  base64.StdEncoding.EncodeToString(saltBytes[:]),
		Iters: 4096,
	}

	client, err := scram.SHA256.NewClient(cred.Username, cred.Password, "")
	if err != nil {
		return scram.StoredCredentials{}, err
	}

	return client.GetStoredCredentialsWithError(kf)
}

// parseStoredSCRAM parses a Postgres-style SCRAM stored secret:
// SCRAM-SHA-256$<iters>:<salt>$<storedKey>:<serverKey>
func parseStoredSCRAM(secret string) (scram.StoredCredentials, error) {
	parts := strings.Split(secret, "$")
	if len(parts) != 3 {
		return scram.StoredCredentials{}, errors.New("invalid SCRAM secret format")
	}

	iterSalt := parts[1]
	keyParts := parts[2]

	iterSaltParts := strings.Split(iterSalt, ":")
	if len(iterSaltParts) != 2 {
		return scram.StoredCredentials{}, errors.New("invalid SCRAM secret iter/salt")
	}

	iters, err := strconv.Atoi(iterSaltParts[0])
	if err != nil {
		return scram.StoredCredentials{}, fmt.Errorf("invalid SCRAM iterations: %w", err)
	}
	salt := iterSaltParts[1]

	keys := strings.Split(keyParts, ":")
	if len(keys) != 2 {
		return scram.StoredCredentials{}, errors.New("invalid SCRAM secret keys")
	}

	storedKey, err := base64.StdEncoding.DecodeString(keys[0])
	if err != nil {
		return scram.StoredCredentials{}, fmt.Errorf("invalid SCRAM stored key: %w", err)
	}

	serverKey, err := base64.StdEncoding.DecodeString(keys[1])
	if err != nil {
		return scram.StoredCredentials{}, fmt.Errorf("invalid SCRAM server key: %w", err)
	}

	return scram.StoredCredentials{
		KeyFactors: scram.KeyFactors{
			Salt:  salt,
			Iters: iters,
		},
		StoredKey: storedKey,
		ServerKey: serverKey,
	}, nil
}

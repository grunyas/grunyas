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

// AuthenticateUser validates the provided credentials against the configured ones.
func (a *Authenticator) AuthenticateUser(user, password string) error {
	if user != a.cred.Username {
		return fmt.Errorf("role \"%s\" does not exist", user)
	}

	switch a.cred.Method {
	case AuthPlain:
		if password != a.cred.Password {
			return fmt.Errorf("invalid password")
		}
	case AuthMD5:
		// Compute hash of the provided plain password to compare with stored MD5
		// Password from client is plain since Startup() uses CleartextPassword request.
		// Note: we'll simply check if the plain password matches the config password for now
		// or implement proper hash comparison if the config contains hashes.
		if password != a.cred.Password {
			return fmt.Errorf("invalid password")
		}
	case AuthScramSHA256:
		// Similar to MD5, if we get a plain password, we compare it.
		if password != a.cred.Password {
			return fmt.Errorf("invalid password")
		}
	}

	return nil
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

package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	AccessTokenTTL        = 15 * time.Minute
	SecurityProofTTL      = 5 * time.Minute
	VideoPlaybackTokenTTL = 5 * time.Minute
	LoginSessionTTL       = 30 * 24 * time.Hour
	RefreshReplayWindow   = 30 * time.Second
	accessTokenUse        = "access"
	securityProofTokenUse = "security_proof"
	videoPlaybackTokenUse = "video_playback"
	authTokenIssuer       = "new-api"
	authTokenAudience     = "new-api-dashboard"
)

var (
	ErrAuthTokenInvalid = errors.New("authentication token is invalid")
	ErrAuthTokenExpired = errors.New("authentication token has expired")
	ErrProofScope       = errors.New("security proof scope mismatch")
	ErrProofMethod      = errors.New("security proof method mismatch")
	ErrPlaybackScope    = errors.New("video playback token scope mismatch")
)

// AuthIdentity is the server-validated identity attached to dashboard requests.
// Role, status and group are deliberately loaded from the user cache instead of JWT claims.
type AuthIdentity struct {
	UserID          int
	SessionID       string
	UserAuthVersion int64
	SessionVersion  int64
}

type authClaims struct {
	TokenUse        string   `json:"token_use"`
	SessionID       string   `json:"sid"`
	UserAuthVersion int64    `json:"uv"`
	SessionVersion  int64    `json:"sv"`
	Method          string   `json:"method,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	TaskID          string   `json:"task_id,omitempty"`
	jwt.RegisteredClaims
}

func authSigningKey(purpose string) []byte {
	mac := hmac.New(sha256.New, []byte(common.SessionSecret))
	_, _ = mac.Write([]byte("new-api/auth/" + purpose + "/v1"))
	return mac.Sum(nil)
}

func IssueAccessToken(identity AuthIdentity) (string, int64, error) {
	if identity.UserID <= 0 || identity.SessionID == "" || identity.UserAuthVersion <= 0 || identity.SessionVersion <= 0 {
		return "", 0, ErrAuthTokenInvalid
	}
	now := time.Now()
	expiresAt := now.Add(AccessTokenTTL)
	claims := authClaims{
		TokenUse:        accessTokenUse,
		SessionID:       identity.SessionID,
		UserAuthVersion: identity.UserAuthVersion,
		SessionVersion:  identity.SessionVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    authTokenIssuer,
			Subject:   strconv.Itoa(identity.UserID),
			Audience:  jwt.ClaimStrings{authTokenAudience},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(authSigningKey(accessTokenUse))
	return signed, expiresAt.Unix(), err
}

func ParseAccessToken(raw string) (AuthIdentity, error) {
	claims, err := parseAuthClaims(raw, accessTokenUse, authSigningKey(accessTokenUse))
	if err != nil {
		return AuthIdentity{}, err
	}
	userID, err := strconv.Atoi(claims.Subject)
	if err != nil || userID <= 0 || claims.SessionID == "" || claims.UserAuthVersion <= 0 || claims.SessionVersion <= 0 {
		return AuthIdentity{}, ErrAuthTokenInvalid
	}
	return AuthIdentity{
		UserID:          userID,
		SessionID:       claims.SessionID,
		UserAuthVersion: claims.UserAuthVersion,
		SessionVersion:  claims.SessionVersion,
	}, nil
}

// IssueVideoPlaybackToken creates a short-lived bearer URL credential scoped
// to exactly one user and one task. It exists so native <video> requests can
// use HTTP Range without exposing the upstream media URL or a dashboard token.
func IssueVideoPlaybackToken(userID int, taskID string) (string, int64, error) {
	taskID = strings.TrimSpace(taskID)
	if userID <= 0 || taskID == "" {
		return "", 0, ErrAuthTokenInvalid
	}
	now := time.Now()
	expiresAt := now.Add(VideoPlaybackTokenTTL)
	claims := authClaims{
		TokenUse: videoPlaybackTokenUse,
		TaskID:   taskID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    authTokenIssuer,
			Subject:   strconv.Itoa(userID),
			Audience:  jwt.ClaimStrings{authTokenAudience},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(authSigningKey(videoPlaybackTokenUse))
	return signed, expiresAt.Unix(), err
}

func ParseVideoPlaybackToken(raw, taskID string) (int, error) {
	claims, err := parseAuthClaims(raw, videoPlaybackTokenUse, authSigningKey(videoPlaybackTokenUse))
	if err != nil {
		return 0, err
	}
	if !hmac.Equal([]byte(claims.TaskID), []byte(strings.TrimSpace(taskID))) {
		return 0, ErrPlaybackScope
	}
	userID, err := strconv.Atoi(claims.Subject)
	if err != nil || userID <= 0 {
		return 0, ErrAuthTokenInvalid
	}
	return userID, nil
}

// ParseDashboardAccessToken distinguishes new-api dashboard JWTs from opaque
// credentials. A token carrying the dashboard issuer, audience and a known
// token use is always treated as internal, even when its signature, lifetime
// or requested purpose is invalid, so it can never fall through to PAT or
// relay-token authentication.
func ParseDashboardAccessToken(raw string) (identity AuthIdentity, internal bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AuthIdentity{}, false, nil
	}
	claims := &authClaims{}
	parsed, _, parseErr := jwt.NewParser().ParseUnverified(raw, claims)
	if parseErr != nil || parsed == nil {
		return AuthIdentity{}, false, nil
	}
	audienceMatches := false
	for _, audience := range claims.Audience {
		if audience == authTokenAudience {
			audienceMatches = true
			break
		}
	}
	knownTokenUse := claims.TokenUse == accessTokenUse || claims.TokenUse == securityProofTokenUse
	if claims.Issuer != authTokenIssuer || !audienceMatches || !knownTokenUse {
		return AuthIdentity{}, false, nil
	}
	identity, err = ParseAccessToken(raw)
	return identity, true, err
}

func IssueSecurityProof(identity AuthIdentity, method string, scopes []string) (string, int64, error) {
	method = strings.TrimSpace(method)
	if identity.UserID <= 0 || identity.SessionID == "" || identity.UserAuthVersion <= 0 || identity.SessionVersion <= 0 || method == "" || len(scopes) == 0 {
		return "", 0, ErrAuthTokenInvalid
	}
	now := time.Now()
	expiresAt := now.Add(SecurityProofTTL)
	claims := authClaims{
		TokenUse:        securityProofTokenUse,
		SessionID:       identity.SessionID,
		UserAuthVersion: identity.UserAuthVersion,
		SessionVersion:  identity.SessionVersion,
		Method:          method,
		Scopes:          append([]string(nil), scopes...),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    authTokenIssuer,
			Subject:   strconv.Itoa(identity.UserID),
			Audience:  jwt.ClaimStrings{authTokenAudience},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(authSigningKey(securityProofTokenUse))
	return signed, expiresAt.Unix(), err
}

func VerifySecurityProof(raw string, identity AuthIdentity, requiredScope string, allowedMethods []string) (string, error) {
	claims, err := parseAuthClaims(raw, securityProofTokenUse, authSigningKey(securityProofTokenUse))
	if err != nil {
		return "", err
	}
	userID, err := strconv.Atoi(claims.Subject)
	if err != nil || userID != identity.UserID || claims.SessionID != identity.SessionID || claims.UserAuthVersion != identity.UserAuthVersion || claims.SessionVersion != identity.SessionVersion {
		return "", ErrAuthTokenInvalid
	}
	methodAllowed := len(allowedMethods) == 0
	for _, method := range allowedMethods {
		if hmac.Equal([]byte(claims.Method), []byte(method)) {
			methodAllowed = true
			break
		}
	}
	if !methodAllowed {
		return "", ErrProofMethod
	}
	if requiredScope != "" {
		found := false
		for _, scope := range claims.Scopes {
			if hmac.Equal([]byte(scope), []byte(requiredScope)) {
				found = true
				break
			}
		}
		if !found {
			return "", ErrProofScope
		}
	}
	return claims.Method, nil
}

func parseAuthClaims(raw, expectedUse string, key []byte) (*authClaims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrAuthTokenInvalid
	}
	claims := &authClaims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("%w: unexpected signing method", ErrAuthTokenInvalid)
		}
		return key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(authTokenIssuer), jwt.WithAudience(authTokenAudience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(5*time.Second))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrAuthTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrAuthTokenInvalid, err)
	}
	if !parsed.Valid || claims.TokenUse != expectedUse || claims.ID == "" || claims.IssuedAt == nil || claims.NotBefore == nil {
		return nil, ErrAuthTokenInvalid
	}
	return claims, nil
}

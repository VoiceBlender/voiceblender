package lkmedia

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JoinClaims captures the LiveKit join-token fields VoiceBlender mints
// when the request omits a pre-signed Token and LIVEKIT_TOKEN_SIGNING is
// enabled. Mirrors the LiveKit JWT claim shape exactly so the resulting
// token is indistinguishable from one minted by the LiveKit CLI.
type JoinClaims struct {
	Identity       string
	Name           string
	Room           string
	TTL            time.Duration
	CanPublish     bool
	CanSubscribe   bool
	CanPublishData bool
	RoomAdmin      bool
}

// videoGrant matches LiveKit's `video` claim object.
type videoGrant struct {
	Room           string `json:"room,omitempty"`
	RoomJoin       bool   `json:"roomJoin,omitempty"`
	RoomAdmin      bool   `json:"roomAdmin,omitempty"`
	CanPublish     *bool  `json:"canPublish,omitempty"`
	CanSubscribe   *bool  `json:"canSubscribe,omitempty"`
	CanPublishData *bool  `json:"canPublishData,omitempty"`
}

// livekitClaims is the JWT payload shape LiveKit expects.
type livekitClaims struct {
	Video videoGrant `json:"video,omitempty"`
	Name  string     `json:"name,omitempty"`
	jwt.RegisteredClaims
}

// MintJoinToken returns a LiveKit-format JWT signed with apiSecret. The
// JWT shape matches the LiveKit Go server-sdk's output (HS256, `iss` =
// API key, `sub` = identity, `exp` from TTL, `video.*` grants).
//
// Validation: apiKey, apiSecret, claims.Identity, and claims.Room must be
// non-empty. TTL <= 0 defaults to 6 hours (LiveKit's recommended max).
func MintJoinToken(apiKey, apiSecret string, c JoinClaims) (string, error) {
	if apiKey == "" {
		return "", errors.New("lkmedia: apiKey is required")
	}
	if apiSecret == "" {
		return "", errors.New("lkmedia: apiSecret is required")
	}
	if c.Identity == "" {
		return "", errors.New("lkmedia: Identity is required")
	}
	if c.Room == "" {
		return "", errors.New("lkmedia: Room is required")
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	now := time.Now()

	v := videoGrant{
		Room:      c.Room,
		RoomJoin:  true,
		RoomAdmin: c.RoomAdmin,
	}
	if c.CanPublish {
		t := true
		v.CanPublish = &t
	}
	if c.CanSubscribe {
		t := true
		v.CanSubscribe = &t
	}
	if c.CanPublishData {
		t := true
		v.CanPublishData = &t
	}

	payload := livekitClaims{
		Video: v,
		Name:  c.Name,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    apiKey,
			Subject:   c.Identity,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, payload)
	signed, err := tok.SignedString([]byte(apiSecret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

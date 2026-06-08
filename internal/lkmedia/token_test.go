package lkmedia

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMintJoinToken_HappyPath(t *testing.T) {
	tok, err := MintJoinToken("api-key-1", "secret", JoinClaims{
		Identity:     "vb-bridge",
		Room:         "support-room",
		Name:         "VoiceBlender",
		CanPublish:   true,
		CanSubscribe: true,
		TTL:          30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("MintJoinToken: %v", err)
	}
	if tok == "" {
		t.Fatal("returned empty token")
	}

	// Verify round-trip parse with the same secret.
	parsed, err := jwt.Parse(tok, func(_ *jwt.Token) (interface{}, error) {
		return []byte("secret"), nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims type = %T", parsed.Claims)
	}
	if got := claims["iss"]; got != "api-key-1" {
		t.Errorf("iss = %v, want api-key-1", got)
	}
	if got := claims["sub"]; got != "vb-bridge" {
		t.Errorf("sub = %v, want vb-bridge", got)
	}
	if got := claims["name"]; got != "VoiceBlender" {
		t.Errorf("name = %v, want VoiceBlender", got)
	}
	video, ok := claims["video"].(map[string]interface{})
	if !ok {
		t.Fatalf("video claim missing or wrong type: %T", claims["video"])
	}
	if video["room"] != "support-room" {
		t.Errorf("video.room = %v", video["room"])
	}
	if video["roomJoin"] != true {
		t.Errorf("video.roomJoin = %v, want true", video["roomJoin"])
	}
	if video["canPublish"] != true {
		t.Errorf("video.canPublish = %v, want true", video["canPublish"])
	}
	if video["canSubscribe"] != true {
		t.Errorf("video.canSubscribe = %v, want true", video["canSubscribe"])
	}
}

func TestMintJoinToken_DefaultTTL(t *testing.T) {
	before := time.Now()
	tok, err := MintJoinToken("k", "s", JoinClaims{Identity: "i", Room: "r"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(tok, func(_ *jwt.Token) (interface{}, error) { return []byte("s"), nil })
	if err != nil {
		t.Fatal(err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	expF, ok := claims["exp"].(float64)
	if !ok {
		t.Fatalf("exp not float: %T", claims["exp"])
	}
	exp := time.Unix(int64(expF), 0)
	delta := exp.Sub(before)
	if delta < 5*time.Hour+50*time.Minute || delta > 6*time.Hour+10*time.Minute {
		t.Errorf("default exp ≈ %v, want ~6h", delta)
	}
}

func TestMintJoinToken_RoomAdmin(t *testing.T) {
	tok, err := MintJoinToken("k", "s", JoinClaims{
		Identity:  "admin",
		Room:      "r",
		RoomAdmin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jwt.Parse(tok, func(_ *jwt.Token) (interface{}, error) { return []byte("s"), nil })
	video := parsed.Claims.(jwt.MapClaims)["video"].(map[string]interface{})
	if video["roomAdmin"] != true {
		t.Errorf("video.roomAdmin = %v, want true", video["roomAdmin"])
	}
}

func TestMintJoinToken_Validation(t *testing.T) {
	cases := []struct {
		name string
		key  string
		sec  string
		c    JoinClaims
	}{
		{"missing key", "", "s", JoinClaims{Identity: "i", Room: "r"}},
		{"missing secret", "k", "", JoinClaims{Identity: "i", Room: "r"}},
		{"missing identity", "k", "s", JoinClaims{Room: "r"}},
		{"missing room", "k", "s", JoinClaims{Identity: "i"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MintJoinToken(tc.key, tc.sec, tc.c); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestMintJoinToken_OmittedGrantsAbsent(t *testing.T) {
	// When CanPublish/CanSubscribe/CanPublishData are false, the JSON
	// claim must OMIT them (LiveKit's server treats absent as "deny" for
	// some grants and as "allow by default" for others — so we don't want
	// to accidentally explicitly set them to false).
	tok, err := MintJoinToken("k", "s", JoinClaims{Identity: "i", Room: "r"})
	if err != nil {
		t.Fatal(err)
	}
	// Decode the payload directly to inspect raw JSON.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token shape: %s", tok)
	}
	parsed, _ := jwt.Parse(tok, func(_ *jwt.Token) (interface{}, error) { return []byte("s"), nil })
	video := parsed.Claims.(jwt.MapClaims)["video"].(map[string]interface{})
	for _, k := range []string{"canPublish", "canSubscribe", "canPublishData"} {
		if _, ok := video[k]; ok {
			t.Errorf("video.%s present when grant was false; should be omitted", k)
		}
	}
}

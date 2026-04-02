package downstream_client

import "testing"

func TestComputeMD5Password(t *testing.T) {
	// PostgreSQL MD5 password: "md5" + md5(md5(password + user) + salt)
	// We can verify against known values.
	user := "postgres"
	password := "secret"
	salt := [4]byte{0x01, 0x02, 0x03, 0x04}

	result := ComputeMD5Password(user, password, salt)

	if result[:3] != "md5" {
		t.Fatalf("expected result to start with 'md5', got %q", result)
	}
	// MD5 hex is 32 chars + "md5" prefix = 35
	if len(result) != 35 {
		t.Fatalf("expected 35 chars, got %d", len(result))
	}

	// Same inputs should produce same output (deterministic)
	result2 := ComputeMD5Password(user, password, salt)
	if result != result2 {
		t.Fatalf("expected deterministic output, got %q and %q", result, result2)
	}

	// Different salt should produce different output
	salt2 := [4]byte{0x05, 0x06, 0x07, 0x08}
	result3 := ComputeMD5Password(user, password, salt2)
	if result == result3 {
		t.Fatalf("expected different result for different salt")
	}

	// Different user should produce different output
	result4 := ComputeMD5Password("other", password, salt)
	if result == result4 {
		t.Fatalf("expected different result for different user")
	}
}

package auth

import "testing"

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "" {
		t.Fatal("empty hash")
	}

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "correct password", input: "correct horse battery staple", want: true},
		{name: "wrong password", input: "Tr0ub4dor&3", want: false},
		{name: "empty password", input: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(tt.input, hash)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if ok != tt.want {
				t.Fatalf("VerifyPassword(%q) = %v, want %v", tt.input, ok, tt.want)
			}
		})
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt missing")
	}
}

package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name string
		pw   string
		want error
	}{
		{name: "valid letters numbers symbols", pw: "Abc123!@#"},
		{name: "too short", pw: "Abc123!", want: ErrPasswordTooShort},
		{name: "too long", pw: strings.Repeat("A", MaxPasswordChars+1), want: ErrPasswordTooLong},
		{name: "space", pw: "Abc123! ", want: ErrPasswordInvalidChars},
		{name: "tab", pw: "Abc123!\t", want: ErrPasswordInvalidChars},
		{name: "chinese", pw: "Abc123!中", want: ErrPasswordInvalidChars},
		{name: "emoji", pw: "Abc123!😀", want: ErrPasswordInvalidChars},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.pw)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ValidatePassword(%q) error = %v, want %v", tt.pw, err, tt.want)
			}
		})
	}
}

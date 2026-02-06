package email

import "testing"

func TestRedactEmail(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard email",
			input: "john@gmail.com",
			want:  "j***@gmail.com",
		},
		{
			name:  "single char local part",
			input: "j@example.com",
			want:  "j***@example.com",
		},
		{
			name:  "long local part",
			input: "longusername@domain.co.uk",
			want:  "l***@domain.co.uk",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "no at sign",
			input: "invalidemail",
			want:  "***",
		},
		{
			name:  "empty local part",
			input: "@domain.com",
			want:  "***@domain.com",
		},
		{
			name:  "multiple at signs only first split",
			input: "user@sub@domain.com",
			want:  "u***@sub@domain.com",
		},
		{
			name:  "special characters in local part",
			input: "+tagged@gmail.com",
			want:  "+***@gmail.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactEmail(tt.input)
			if got != tt.want {
				t.Errorf("RedactEmail(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

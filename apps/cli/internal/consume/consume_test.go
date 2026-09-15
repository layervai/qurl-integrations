package consume

import (
	"errors"
	"testing"
)

// TestDecide pins the full §16.2 decision table: the action is a pure
// function of stdout's TTY-ness and --file, and the piped-without-file cell
// is the only refusal.
func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		tty     bool
		file    string
		want    Action
		wantErr error
	}{
		{name: "tty no file opens browser", tty: true, file: "", want: ActionOpenBrowser},
		{name: "piped no file refuses", tty: false, file: "", wantErr: ErrPipedNeedsFile},
		{name: "tty dash streams stdout", tty: true, file: "-", want: ActionStreamStdout},
		{name: "piped dash streams stdout", tty: false, file: "-", want: ActionStreamStdout},
		{name: "tty file saves", tty: true, file: "out.bin", want: ActionSaveFile},
		{name: "piped file saves", tty: false, file: "out.bin", want: ActionSaveFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.tty, tc.file)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Decide(%t, %q) err = %v, want %v", tc.tty, tc.file, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide(%t, %q) err = %v", tc.tty, tc.file, err)
			}
			if got != tc.want {
				t.Fatalf("Decide(%t, %q) = %d, want %d", tc.tty, tc.file, got, tc.want)
			}
		})
	}
}

// TestCustomerMessagesNonEmpty guards the jargon-gate feed: every registered
// message must be a real string.
func TestCustomerMessagesNonEmpty(t *testing.T) {
	msgs := CustomerMessages()
	if len(msgs) == 0 {
		t.Fatal("no customer messages registered")
	}
	for i, m := range msgs {
		if m == "" {
			t.Errorf("customer message %d is empty", i)
		}
	}
}

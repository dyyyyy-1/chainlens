package indexer

import "testing"

func TestNextRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		nextBlock uint64
		head      uint64
		batch     uint64
		want      Range
		wantOK    bool
	}{
		{
			name:      "batch limited by configured size",
			nextBlock: 100,
			head:      10000,
			batch:     100,
			want:      Range{From: 100, To: 199},
			wantOK:    true,
		},
		{
			name:      "batch limited by chain head",
			nextBlock: 100,
			head:      150,
			batch:     2000,
			want:      Range{From: 100, To: 150},
			wantOK:    true,
		},
		{
			name:      "single block at head",
			nextBlock: 150,
			head:      150,
			batch:     2000,
			want:      Range{From: 150, To: 150},
			wantOK:    true,
		},
		{
			name:      "caught up with no work",
			nextBlock: 151,
			head:      150,
			batch:     2000,
			want:      Range{},
			wantOK:    false,
		},
		{
			name:      "zero batch degrades to single block",
			nextBlock: 100,
			head:      200,
			batch:     0,
			want:      Range{From: 100, To: 100},
			wantOK:    true,
		},
		{
			name:      "genesis head",
			nextBlock: 0,
			head:      0,
			batch:     10,
			want:      Range{From: 0, To: 0},
			wantOK:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, gotOK := nextRange(test.nextBlock, test.head, test.batch)
			if gotOK != test.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, test.wantOK)
			}
			if got != test.want {
				t.Fatalf("range = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestSafeBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		head          uint64
		confirmations uint64
		want          uint64
		wantOK        bool
	}{
		{name: "typical", head: 200, confirmations: 12, want: 188, wantOK: true},
		{name: "head equals confirmations", head: 12, confirmations: 12, want: 0, wantOK: false},
		{name: "head below confirmations", head: 5, confirmations: 12, want: 0, wantOK: false},
		{name: "just above threshold", head: 13, confirmations: 12, want: 1, wantOK: true},
		{name: "zero confirmations", head: 100, confirmations: 0, want: 100, wantOK: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, gotOK := safeBlock(test.head, test.confirmations)
			if got != test.want {
				t.Fatalf("block = %d, want %d", got, test.want)
			}
			if gotOK != test.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, test.wantOK)
			}
		})
	}
}

package indexer

import "testing"

func TestNextRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		nextBlock     uint64
		head          uint64
		confirmations uint64
		batch         uint64
		want          Range
		wantOK        bool
	}{
		{
			name:          "batch limited by configured size",
			nextBlock:     100,
			head:          10000,
			confirmations: 12,
			batch:         100,
			want:          Range{From: 100, To: 199},
			wantOK:        true,
		},
		{
			name:          "batch limited by confirmation watermark",
			nextBlock:     100,
			head:          200,
			confirmations: 12,
			batch:         2000,
			want:          Range{From: 100, To: 188},
			wantOK:        true,
		},
		{
			name:          "exactly at watermark yields single block",
			nextBlock:     188,
			head:          200,
			confirmations: 12,
			batch:         2000,
			want:          Range{From: 188, To: 188},
			wantOK:        true,
		},
		{
			name:          "caught up with no work",
			nextBlock:     189,
			head:          200,
			confirmations: 12,
			batch:         2000,
			want:          Range{},
			wantOK:        false,
		},
		{
			name:          "head below confirmation threshold",
			nextBlock:     1,
			head:          5,
			confirmations: 12,
			batch:         100,
			want:          Range{},
			wantOK:        false,
		},
		{
			name:          "zero batch degrades to single block",
			nextBlock:     100,
			head:          200,
			confirmations: 0,
			batch:         0,
			want:          Range{From: 100, To: 100},
			wantOK:        true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, gotOK := nextRange(test.nextBlock, test.head, test.confirmations, test.batch)
			if gotOK != test.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, test.wantOK)
			}
			if got != test.want {
				t.Fatalf("range = %+v, want %+v", got, test.want)
			}
		})
	}
}

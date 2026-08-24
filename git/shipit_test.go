package git

import "testing"

func TestOwnLineShipitIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "own line with fb prefix",
			body: "subject\n\nfbshipit-source-id: 0123456789abcdef0123456789abcdef01234567\n",
			want: []string{"0123456789abcdef0123456789abcdef01234567"},
		},
		{
			name: "own line without fb prefix",
			body: "subject\n\nshipit-source-id: 0fedcba\n",
			want: []string{"0fedcba"},
		},
		{
			name: "indented own line (post-dedent form)",
			body: "subject\n\n  shipit-source-id: 1234567\n",
			want: []string{"1234567"},
		},
		{
			name: "multiple ids",
			body: "squash of three\n\nfbshipit-source-id: 0aaaaaa\nfbshipit-source-id: 1bbbbbb\nshipit-source-id: 2cccccc\n",
			want: []string{"0aaaaaa", "1bbbbbb", "2cccccc"},
		},
		{
			name: "quoted mid-prose is ignored",
			body: "revert of the change recorded as shipit-source-id: 05220186 mid-sentence.\n",
			want: nil,
		},
		{
			name: "trailing prose on the same line is ignored",
			body: "see fbshipit-source-id: 05220186 above\n",
			want: nil,
		},
		{
			name: "no ids",
			body: "plain message\n",
			want: nil,
		},
	} {
		c := &Commit{Body: tc.body}
		got := c.OwnLineShipitIDs()
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got[%d] = %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

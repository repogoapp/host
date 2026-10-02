package git

import "testing"

func TestParseRemote(t *testing.T) {
	cases := []struct{ url, want string }{
		{"git@github.com:octocat/code.git", "octocat/code"},
		{"https://github.com/octocat/code.git", "octocat/code"},
		{"https://github.com/octocat/code", "octocat/code"},
		{"ssh://git@github.com/octocat/code.git", "octocat/code"},
		{"ssh://git@host:2222/octocat/code.git", "octocat/code"},
		{"https://gitlab.com/octocat/code.git", "octocat/code"},
		{"git@gitea.example.com:octocat/code.git", "octocat/code"},
		{"https://github.com/octocat/code/", "octocat/code"},
		// A subgroup keeps the two segments nearest the repository, which is
		// what identifies it.
		{"https://gitlab.com/group/sub/code.git", "sub/code"},
		// Nothing that reduces to owner/repo.
		{"", ""},
		{"/srv/mirrors/code.git", "mirrors/code"},
		{"code.git", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := ParseRemote(c.url); got != c.want {
			t.Errorf("ParseRemote(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

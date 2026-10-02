package github

import ghcore "github.com/repogo/host/internal/github"

type PathParams struct {
	Path string `json:"path"`
}

type AvatarParams struct {
	IfNoneMatch string `json:"if_none_match"`
}

type ReposParams struct {
	// Owner selects a user or org; empty is the signed-in account.
	Owner string `json:"owner"`
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type ReposResult struct {
	Repos []ghcore.Repo `json:"repos" wire:"array"`
}

type CloneParams struct {
	NameWithOwner string `json:"name_with_owner"`
}

type PRCreateParams struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// PublishParams' name is a bare repository name; the owner is always the
// host's signed-in account.
type PublishParams struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
}

package conformance_test

import (
	"testing"

	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/swiftcontract"
	"github.com/repogo/host/internal/testhost"
)

const swiftServing = "../../../../swift/Packages/RepoGit/Sources/LocalRPC/Generated/HostServing.swift"

// LocalRPC answers these on the phone; the other methods it serves have
// nothing to report there and answer their zero value, so need no types.
var (
	servedMethods = []string{
		"git.status", "git.changes", "git.branches", "git.diff", "git.patch", "git.pull",
		"git.reset_hard", "git.switch_branch", "git.create_branch", "git.commit_push",
		"fs.list", "fs.read", "fs.write", "fs.replace", "fs.search", "fs.delete", "fs.rename",
		"fs.mkdir", "fs.new_project", "fs.add_project", "fs.watch", "fs.stop",
		"project.list", "project.detect_icon", "project.rename", "project.pin", "host.status", "host.setup",
		"github.repos", "github.clone", "github.publish", "github.pr_create", "github.avatar",
		"chats.list", "sync.pull",
		"tools.login_start", "tools.login_complete", "tools.login_cancel", "tools.logins", "tools.logout",
		"chats.start", "chats.send", "chats.messages", "chats.subscribe", "chats.unsubscribe", "chats.info",
		"chats.queue", "chats.update", "chats.resolve", "chats.delete", "chats.tools_subscribe", "chats.tools_unsubscribe",
		"turns.get", "turns.stop", "turns.list",
		"ports.list", "forward.pipe_open", "forward.pipe_send", "forward.pipe_close",
	}
	servedEvents = []string{"git.changed", "fs.change", "project.changed",
		"chats.appended", "chats.streaming", "chats.changed", "chats.attention", "chats.removed",
		"forward.pipe_data", "forward.pipe_closed", "project.changed"}
)

// The phone's local handlers answer with the host's own shapes.
func TestSwiftServingMatchesRouter(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../../swift")
	got := swiftcontract.Serving(newHost(t).router.Specs(), emit.Catalog(), servedMethods, servedEvents)
	testhost.Golden(t, swiftServing, []byte(got), *update, updateHint)
}

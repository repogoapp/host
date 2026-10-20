package tunnelwire

import "testing"

func TestEnvHostID(t *testing.T) {
	id := "bbe07c2289f188a6d7b4331f63f875f4"
	if got, ok := EnvHostID(EnvLabel(id)); !ok || got != id {
		t.Fatalf("%q %v", got, ok)
	}
	for _, label := range []string{id, "h-" + id[:31], "h-" + id + "0", "h-BBE07C2289F188A6D7B4331F63F875F4", "x-" + id} {
		if _, ok := EnvHostID(label); ok {
			t.Errorf("%q read as an environment label", label)
		}
	}
}

func TestParsePluginPath(t *testing.T) {
	for path, want := range map[string]PluginRoute{
		"/deploys/notify":                    {Plugin: "deploys", Method: "notify"},
		"/builds/install/abc/manifest.plist": {Plugin: "builds", Method: "install", Rest: "abc/manifest.plist"},
		"/builds/install/":                   {Plugin: "builds", Method: "install"},
		"/cron-lab/run_now":                  {Plugin: "cron-lab", Method: "run_now"},
	} {
		if got, ok := ParsePluginPath(path); !ok || got != want {
			t.Errorf("%s: %+v %v", path, got, ok)
		}
	}
	for _, path := range []string{"", "/", "/builds", "/builds/", "builds/install", "/Builds/install", "/../install", "/builds/.list", "/builds/tasks.list"} {
		if _, ok := ParsePluginPath(path); ok {
			t.Errorf("%q parsed", path)
		}
	}
}

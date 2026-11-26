package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	rpcscreens "github.com/repogoapp/host/internal/rpc/screens"
	"github.com/repogoapp/host/internal/screens"
)

// `repogo screens` drives this machine's simulators and emulators through the
// running host's screens.* methods: the same ones the phone and agents call.

// screensCommands are `repogo screens`' own commands, for its help and usage.
const screensCommands = `Commands:
  list                          every simulator and emulator, booted first
  elements [--json]             what's on screen: ref, role, label, value, frame
  tap <ref | label | X Y>       tap an element by ref or label, or a point
  wait --label T | --gone T | --stable [--timeout MS]
                                read until it's so; prints the screen
  swipe X1 Y1 X2 Y2 [--ms 300]  drag one finger
  type <text>                   type into the focused field
  key <name>                    return, delete, tab, arrows...; key alone lists all
  button <name>                 home, back, recents, app_switcher, lock, volume_up, volume_down
  rotate left|right             turn a quarter
  fold closed|book|open|laptop  set a foldable's pose
  open-url <url>                open a link, or an app's URL scheme
  launch <app> [--grant P,P]    open an app by bundle ID (iOS) or package (Android),
                                granting permissions first (camera, photos, location...)
  screenshot [FILE]             save a PNG screenshot
  recording start               start recording the screen
  recording stop [FILE]         stop, and save the video
  boot <screen>                 boot one and wait until it's ready
  shutdown <screen>             shut one down

Points on iOS, pixels on Android, as elements reports them. Without --screen,
the one booted simulator or emulator.`

const screensUsage = "usage: repogo screens <command> [--screen ID]\n" + screensCommands

func screensFlags(fs *flag.FlagSet) {
	fs.String("screen", "", "the simulator or emulator, by its id in repogo screens list")
	fs.Bool("json", false, "list, elements, wait: print the host's answer as JSON")
	fs.Int("ms", 300, "swipe: how long it takes, in milliseconds")
	fs.String("label", "", "wait: until an element's label, id or field text contains this")
	fs.String("gone", "", "wait: until no element's does")
	fs.Bool("stable", false, "wait: until two reads in a row are the same")
	fs.Int("timeout", 10000, "wait: how long to wait at most, in milliseconds")
	fs.String("grant", "", "launch: permissions to grant first, comma-separated: "+strings.Join(screens.GrantNames(), ", "))
}

func screensCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", screensUsage)
	}
	sub, args := args[0], args[1:]
	asJSON := flagBool(fs, "json")
	usage := func(form string) error { return fmt.Errorf("usage: repogo screens %s", form) }
	// actWithin runs one screens.* method on the picked screen with more
	// params, waiting up to timeout for its answer; act waits as call does.
	actWithin := func(timeout time.Duration, method string, params map[string]any, out any) error {
		id, err := pickScreen(ctx, flagString(fs, "screen"))
		if err != nil {
			return err
		}
		params["screen"] = id
		return callWithin(ctx, timeout, method, params, out)
	}
	act := func(method string, params map[string]any, out any) error {
		return actWithin(8*time.Second, method, params, out)
	}

	switch sub {
	case "list":
		if len(args) != 0 {
			return usage("list")
		}
		var list rpcscreens.ListResult
		if err := call(ctx, "screens.list", nil, &list); err != nil {
			return err
		}
		if asJSON {
			return printJSON(list)
		}
		printScreens(os.Stdout, list.Screens)
		return nil

	case "elements":
		if len(args) != 0 {
			return usage("elements [--json]")
		}
		var read screens.Elements
		if err := act("screens.elements", map[string]any{}, &read); err != nil {
			return err
		}
		if asJSON {
			return printJSON(read)
		}
		printElements(os.Stdout, read)
		return nil

	case "tap":
		params := map[string]any{}
		switch {
		case len(args) == 2 && isNumber(args[0]) && isNumber(args[1]):
			x, _ := strconv.ParseFloat(args[0], 64)
			y, _ := strconv.ParseFloat(args[1], 64)
			params["x"], params["y"] = x, y
		case len(args) == 1 && isRef(args[0]):
			params["ref"] = args[0]
		case len(args) >= 1:
			params["label"] = strings.Join(args, " ")
		default:
			return usage("tap <ref | label | X Y>")
		}
		var tapped rpcscreens.Tapped
		if err := act("screens.tap", params, &tapped); err != nil {
			return err
		}
		fmt.Printf("Tapped %s, %s\n", trimFloat(tapped.X), trimFloat(tapped.Y))
		return nil

	case "wait":
		if len(args) != 0 {
			return usage("wait --label T | --gone T | --stable [--timeout MS]")
		}
		timeout, _ := strconv.Atoi(flagString(fs, "timeout"))
		params := map[string]any{"label": flagString(fs, "label"), "gone": flagString(fs, "gone"),
			"stable": flagBool(fs, "stable"), "timeout_ms": timeout}
		var waited screens.Waited
		if err := actWithin(time.Duration(timeout)*time.Millisecond+30*time.Second, "screens.wait", params, &waited); err != nil {
			return err
		}
		if asJSON {
			return printJSON(waited)
		}
		fmt.Printf("%s after %d ms\n", map[bool]string{true: "Met", false: "Not met"}[waited.Met], waited.WaitedMS)
		printElements(os.Stdout, waited.Read)
		if !waited.Met {
			return fmt.Errorf("timed out")
		}
		return nil

	case "swipe":
		if len(args) != 4 {
			return usage("swipe X1 Y1 X2 Y2 [--ms 300]")
		}
		var p [4]float64
		for i, a := range args {
			v, err := strconv.ParseFloat(a, 64)
			if err != nil {
				return usage("swipe X1 Y1 X2 Y2 [--ms 300]")
			}
			p[i] = v
		}
		ms, _ := strconv.Atoi(flagString(fs, "ms"))
		return act("screens.swipe", map[string]any{"from_x": p[0], "from_y": p[1], "to_x": p[2], "to_y": p[3], "ms": ms}, nil)

	case "type":
		if len(args) == 0 {
			return usage("type <text>")
		}
		return act("screens.type", map[string]any{"text": strings.Join(args, " ")}, nil)

	case "key":
		if len(args) != 1 {
			return usage("key <" + strings.Join(screens.KeyNames(), "|") + ">")
		}
		return act("screens.key", map[string]any{"key": args[0]}, nil)

	case "button":
		if len(args) != 1 {
			return usage("button <name>")
		}
		return act("screens.button", map[string]any{"button": args[0]}, nil)

	case "rotate":
		if len(args) != 1 || (args[0] != string(screens.TurnLeft) && args[0] != string(screens.TurnRight)) {
			return usage("rotate left|right")
		}
		return act("screens.rotate", map[string]any{"turn": args[0]}, nil)

	case "fold":
		if len(args) != 1 {
			return usage("fold closed|book|open|laptop")
		}
		return act("screens.fold", map[string]any{"pose": args[0]}, nil)

	case "open-url":
		if len(args) != 1 {
			return usage("open-url <url>")
		}
		return act("screens.open_url", map[string]any{"url": args[0]}, nil)

	case "launch":
		if len(args) != 1 {
			return usage("launch <bundle id | package>")
		}
		params := map[string]any{"app": args[0]}
		if grant := flagString(fs, "grant"); grant != "" {
			params["grant"] = strings.Split(grant, ",")
		}
		return act("screens.launch", params, nil)

	case "screenshot":
		if len(args) > 1 {
			return usage("screenshot [FILE]")
		}
		var saved rpcscreens.SavedScreenshot
		if err := act("screens.save_screenshot", map[string]any{}, &saved); err != nil {
			return err
		}
		return deliver(saved.Path, args)

	case "recording":
		switch {
		case len(args) == 1 && args[0] == "start":
			if err := act("screens.recording.start", map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Println("Recording. Stop it with repogo screens recording stop [FILE].")
			return nil
		case len(args) >= 1 && len(args) <= 2 && args[0] == "stop":
			args = args[1:]
		default:
			return usage("recording start | recording stop [FILE]")
		}
		var saved rpcscreens.SavedRecording
		// Finishing the file can take a few seconds.
		if err := actWithin(90*time.Second, "screens.recording.stop", map[string]any{}, &saved); err != nil {
			return err
		}
		return deliver(saved.Path, args)

	case "boot", "shutdown":
		if len(args) != 1 {
			return usage(sub + " <screen>")
		}
		timeout := 8 * time.Second
		if sub == "boot" {
			timeout = 2 * time.Minute
		}
		return callWithin(ctx, timeout, "screens."+sub, map[string]any{"screen": args[0]}, nil)
	}
	return fmt.Errorf("unknown screens command %q\n%s", sub, screensUsage)
}

// pickScreen is the screen named, or the one booted simulator or emulator.
func pickScreen(ctx context.Context, named string) (string, error) {
	if named != "" {
		return named, nil
	}
	var list rpcscreens.ListResult
	if err := call(ctx, "screens.list", nil, &list); err != nil {
		return "", err
	}
	var booted []screens.Screen
	for _, s := range list.Screens {
		if s.Booted() && s.Kind != screens.KindDisplay {
			booted = append(booted, s)
		}
	}
	switch len(booted) {
	case 1:
		return booted[0].ID, nil
	case 0:
		return "", fmt.Errorf("no simulator or emulator is booted: repogo screens boot <screen>, from repogo screens list")
	}
	var b strings.Builder
	printScreens(&b, booted)
	return "", fmt.Errorf("%d are booted; name one with --screen:\n%s", len(booted), strings.TrimRight(b.String(), "\n"))
}

func printScreens(w io.Writer, list []screens.Screen) {
	for _, booted := range []bool{true, false} {
		for _, s := range list {
			if s.Booted() != booted || s.Kind == screens.KindDisplay {
				continue
			}
			state := "  "
			if booted {
				state = "● "
			}
			fmt.Fprintf(w, "%s%-28s %s %s  %s\n", state, s.Name, s.OS.Name, s.OS.Version, s.ID)
		}
	}
}

func printElements(w io.Writer, read screens.Elements) {
	app := ""
	if read.App != "" {
		app = read.App + ", "
	}
	more := ""
	if read.Pose != nil {
		more += ", " + string(*read.Pose)
	}
	if read.Keyboard {
		more += ", keyboard up"
	}
	fmt.Fprintf(w, "%s%s×%s %s, %d elements%s\n", app, trimFloat(read.Width), trimFloat(read.Height), read.Unit,
		len(read.Elements), more)
	for _, e := range read.Elements {
		fmt.Fprintln(w, e.Line())
	}
}

// deliver prints where the host saved a file, or copies it to the one path
// given: the host wrote it, and the copy goes where this shell is.
func deliver(saved string, to []string) error {
	if len(to) == 0 {
		fmt.Println(saved)
		return nil
	}
	if err := copyFile(saved, to[0]); err != nil {
		return err
	}
	os.Remove(saved)
	fmt.Println(to[0])
	return nil
}

func printJSON(v any) error {
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	return out.Encode(v)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// isRef is whether s names an element of a read, as e7.12.
func isRef(s string) bool {
	head, tail, ok := strings.Cut(strings.TrimPrefix(s, "e"), ".")
	if !ok || !strings.HasPrefix(s, "e") {
		return false
	}
	_, err1 := strconv.Atoi(head)
	_, err2 := strconv.Atoi(tail)
	return err1 == nil && err2 == nil
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

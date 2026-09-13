// Window and input commands.
//
// Every command here maps onto one operation of the interaction service and
// returns the revision it produced, so an agent always knows which revision its
// next coordinate input should name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/bnema/wlvision/internal/agentapi"
	"github.com/bnema/wlvision/internal/export"
	"github.com/bnema/wlvision/internal/input"
	"github.com/bnema/wlvision/internal/result"
	"github.com/bnema/wlvision/internal/session"
)

// interactionService builds the interaction service of one session.
func (c *cli) interactionService(id string) (*input.Service, error) {
	service, err := c.newService()
	if err != nil {
		return nil, err
	}
	return input.NewService(service.Caller(id)), nil
}

// exportTree resolves the artifacts directory of one session.
func (c *cli) exportTree(service *session.Service, id string) (*export.Tree, error) {
	return export.Open(service.StateRoot(), id)
}

// windowTarget collects the --window and --revision flags every window command
// takes.
type windowTarget struct {
	handle   string
	revision uint64
}

func (t *windowTarget) register(flags *flag.FlagSet) {
	flags.StringVar(&t.handle, "window", "", "window handle")
	flags.Uint64Var(&t.revision, "revision", 0, "the revision the caller saw; the request is refused if the layout moved")
}

func (t windowTarget) target() input.Target {
	return input.Target{Handle: t.handle, Revision: t.revision}
}

// sessionArgs registers the shared --session flag and the command's own flags,
// then parses the command line. The identifier is returned after parsing, so it
// is the value the caller actually supplied.
func (c *cli) sessionArgs(name string, args []string, extra func(*flag.FlagSet)) (string, *flag.FlagSet, error) {
	var id string
	flags := c.flagSet(name)
	flags.StringVar(&id, "session", "", "session identifier")
	if extra != nil {
		extra(flags)
	}
	if err := flags.Parse(args); err != nil {
		return id, flags, err
	}
	return id, flags, nil
}

func (c *cli) windows(args []string) int {
	const operation = "windows"
	id, flags, err := c.sessionArgs("wlvision windows", args, nil)
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	state, err := service.Windows(c.ctx)
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, state.Revision, state)
}

func (c *cli) activate(args []string) int {
	const operation = "activate"
	var target windowTarget
	id, flags, err := c.sessionArgs("wlvision activate", args, target.register)
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if target.handle == "" {
		return c.usageError(operation, id, errors.New("--window is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	state, err := service.Activate(c.ctx, target.target())
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, state.Revision, state)
}

func (c *cli) move(args []string) int {
	const operation = "move"
	var (
		target windowTarget
		x, y   float64
	)
	id, flags, err := c.sessionArgs("wlvision move", args, func(flags *flag.FlagSet) {
		target.register(flags)
		flags.Float64Var(&x, "x", math.NaN(), "target x in output coordinates")
		flags.Float64Var(&y, "y", math.NaN(), "target y in output coordinates")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if target.handle == "" {
		return c.usageError(operation, id, errors.New("--window is required"))
	}
	if math.IsNaN(x) || math.IsNaN(y) {
		return c.usageError(operation, id, errors.New("--x and --y are required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	state, err := service.Move(c.ctx, input.MoveRequest{Target: target.target(), X: x, Y: y})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, state.Revision, state)
}

func (c *cli) resize(args []string) int {
	const operation = "resize"
	var (
		target        windowTarget
		width, height uint
		timeout       time.Duration
	)
	id, flags, err := c.sessionArgs("wlvision resize", args, func(flags *flag.FlagSet) {
		target.register(flags)
		flags.UintVar(&width, "width", 0, "requested width in logical pixels")
		flags.UintVar(&height, "height", 0, "requested height in logical pixels")
		flags.Var(durationFlag{&timeout}, "timeout", "how long to wait for the application to commit the resize")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if target.handle == "" {
		return c.usageError(operation, id, errors.New("--window is required"))
	}
	if width == 0 || height == 0 {
		return c.usageError(operation, id, errors.New("--width and --height are required and must not be zero"))
	}
	if width > math.MaxUint32 || height > math.MaxUint32 {
		return c.usageError(operation, id, errors.New("--width and --height must fit in 32 bits"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	// A resize completes only once the application committed a matching buffer,
	// so a successful result is the size taking effect, not a configure being
	// sent. A caller that wants to bound how long it waits for that commit passes
	// --timeout; the failure then reports what the session observed.
	ctx := c.ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	resized, err := service.Resize(ctx, input.ResizeRequest{
		Target: target.target(),
		Width:  uint32(width),
		Height: uint32(height),
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeWaitTimeout)
	}
	return c.success(operation, id, resized.Revision, resized)
}

func (c *cli) closeWindow(args []string) int {
	const operation = "close-window"
	var target windowTarget
	id, flags, err := c.sessionArgs("wlvision close-window", args, target.register)
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if target.handle == "" {
		return c.usageError(operation, id, errors.New("--window is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	state, err := service.CloseWindow(c.ctx, target.target())
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, state.Revision, state)
}

func (c *cli) click(args []string) int {
	const operation = "click"
	var (
		target windowTarget
		x, y   float64
		button uint
	)
	id, flags, err := c.sessionArgs("wlvision click", args, func(flags *flag.FlagSet) {
		target.register(flags)
		flags.Float64Var(&x, "x", math.NaN(), "x in window-content coordinates")
		flags.Float64Var(&y, "y", math.NaN(), "y in window-content coordinates")
		flags.UintVar(&button, "button", 0, "pointer button (default left)")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if target.handle == "" {
		return c.usageError(operation, id, errors.New("--window is required"))
	}
	if math.IsNaN(x) || math.IsNaN(y) {
		return c.usageError(operation, id, errors.New("--x and --y are required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	clicked, err := service.Click(c.ctx, input.ClickRequest{
		Target: target.target(),
		X:      x,
		Y:      y,
		Button: uint32(button),
	})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, clicked.Revision, clicked)
}

func (c *cli) pointer(args []string) int {
	const operation = "pointer"
	var (
		x, y, value float64
		button      uint
		axis        uint
		state       string
	)
	id, flags, err := c.sessionArgs("wlvision pointer", args, func(flags *flag.FlagSet) {
		flags.Float64Var(&x, "x", math.NaN(), "absolute x in output coordinates")
		flags.Float64Var(&y, "y", math.NaN(), "absolute y in output coordinates")
		flags.UintVar(&button, "button", 0, "pointer button")
		flags.UintVar(&axis, "axis", 0, "scroll axis: 0 vertical, 1 horizontal")
		flags.Float64Var(&value, "value", 0, "scroll value")
		flags.StringVar(&state, "state", "", "button state: pressed or released")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}

	given := givenFlags(flags)
	request := input.PointerRequest{State: state}
	switch {
	case given["x"] || given["y"]:
		if !given["x"] || !given["y"] {
			return c.usageError(operation, id, errors.New("--x and --y belong together"))
		}
		request.Kind = agentapi.PointerMotion
		request.X, request.Y = x, y
	case given["button"] || given["state"]:
		if !given["button"] || state == "" {
			return c.usageError(operation, id, errors.New("--button requires --state pressed or released"))
		}
		request.Kind = agentapi.PointerButton
		request.Button = uint32(button)
	case given["axis"] || given["value"]:
		if !given["value"] {
			return c.usageError(operation, id, errors.New("--axis requires --value"))
		}
		request.Kind = agentapi.PointerAxis
		request.Axis = uint32(axis)
		request.Value = value
	default:
		return c.usageError(operation, id, errors.New("one of motion (--x/--y), a button (--button/--state) or an axis (--axis/--value) is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	if err := service.Pointer(c.ctx, request); err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, pointerResult{Kind: request.Kind})
}

// pointerResult reports which pointer action was sent.
type pointerResult struct {
	Kind string `json:"kind"`
}

func (c *cli) scroll(args []string) int {
	const operation = "scroll"
	var (
		axis  uint
		value float64
	)
	id, flags, err := c.sessionArgs("wlvision scroll", args, func(flags *flag.FlagSet) {
		flags.UintVar(&axis, "axis", 0, "scroll axis: 0 vertical, 1 horizontal")
		flags.Float64Var(&value, "value", 0, "scroll steps")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if !givenFlags(flags)["value"] {
		return c.usageError(operation, id, errors.New("--value is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	if err := service.Scroll(c.ctx, input.ScrollRequest{Axis: uint32(axis), Value: value}); err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, scrollResult{Axis: axis, Value: value})
}

// scrollResult reports the scroll step that was sent.
type scrollResult struct {
	Axis  uint    `json:"axis"`
	Value float64 `json:"value"`
}

func (c *cli) key(args []string) int {
	const operation = "key"
	var (
		keycode uint
		name    string
		state   string
	)
	id, flags, err := c.sessionArgs("wlvision key", args, func(flags *flag.FlagSet) {
		flags.UintVar(&keycode, "keycode", 0, "evdev keycode")
		flags.StringVar(&name, "name", "", "key name, such as Return or Escape")
		flags.StringVar(&state, "state", "", "send one transition instead of a press and release: pressed or released")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if code := c.checkSessionArgs(operation, id, flags); code != 0 {
		return code
	}
	if (keycode == 0) == (name == "") {
		return c.usageError(operation, id, errors.New("exactly one of --keycode or --name is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	request := input.KeyRequest{Keycode: uint32(keycode), Name: name, State: state}
	if state == "" {
		if err := service.Tap(c.ctx, request); err != nil {
			return c.fail(operation, id, err, result.CodeSessionNotReady)
		}
		return c.success(operation, id, 0, keyResult{Keycode: request.Keycode, Name: name, Tapped: true})
	}
	if err := service.Key(c.ctx, request); err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, keyResult{Keycode: request.Keycode, Name: name, State: state})
}

// keyResult reports which key transition was sent.
type keyResult struct {
	Keycode uint32 `json:"keycode,omitempty"`
	Name    string `json:"name,omitempty"`
	State   string `json:"state,omitempty"`
	Tapped  bool   `json:"tapped,omitempty"`
}

func (c *cli) typeText(args []string) int {
	const operation = "type"
	var text string
	id, flags, err := c.sessionArgs("wlvision type", args, func(flags *flag.FlagSet) {
		flags.StringVar(&text, "text", "", "text to type")
	})
	if err != nil {
		return c.usageError(operation, id, err)
	}
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	// The text is either the --text flag or one positional argument, which is
	// why this command does not use the shared positional check.
	switch {
	case text != "" && flags.NArg() > 0:
		return c.usageError(operation, id, errors.New("--text and a positional text are mutually exclusive"))
	case flags.NArg() > 1:
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(1)))
	case flags.NArg() == 1:
		text = flags.Arg(0)
	case text == "":
		return c.usageError(operation, id, errors.New("the text to type is required"))
	}

	service, err := c.interactionService(id)
	if err != nil {
		return c.fail(operation, id, err, result.CodeEngineUnavailable)
	}
	typed, err := service.Type(c.ctx, input.TypeRequest{Text: text})
	if err != nil {
		return c.fail(operation, id, err, result.CodeSessionNotReady)
	}
	return c.success(operation, id, 0, typed)
}

// checkSessionArgs rejects the two command-line mistakes every command shares:
// a missing session and an unexpected positional argument.
func (c *cli) checkSessionArgs(operation, id string, flags *flag.FlagSet) int {
	if id == "" {
		return c.usageError(operation, "", errors.New("--session is required"))
	}
	if flags.NArg() > 0 {
		return c.usageError(operation, id, fmt.Errorf("unexpected argument %q", flags.Arg(0)))
	}
	return 0
}

// givenFlags reports which flags the caller actually supplied, which is how a
// command with several alternative shapes tells them apart.
func givenFlags(flags *flag.FlagSet) map[string]bool {
	given := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}

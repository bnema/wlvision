// Package skill gates the agent skill against the CLI it documents.
//
// The skill and its scenarios are prose, but every command line they show must
// be one the CLI accepts, or an agent following them fails. This package parses
// the skill documents, extracts every `wlvision ...` command line, and checks it
// against the CLI's own source: the dispatch in cmd/wlvision, the flag sets each
// command registers, and the codes, envelope keys, and JSON fields the
// documents name.
//
// The checks are static and deterministic: they read Go and Markdown files, and
// they never build an image, start a container, or run the CLI.
package skill

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// skillDocuments are the documents whose command lines and contract references
// are gated. The human documentation is gated only for placeholders.
var skillDocuments = []string{
	filepath.Join("skills", "wlvision", "SKILL.md"),
	filepath.Join("skills", "wlvision", "tests", "scenarios.md"),
}

// cliModel is the subset of the CLI this gate needs: the commands it dispatches,
// the flags each command accepts, the global flags, and the usage text.
type cliModel struct {
	// commands maps "doctor" and "session create" to their flag sets.
	commands map[string]*commandFlags
	// usage is the rendered usage text of cmd/wlvision/main.go.
	usage string
	// globals maps a global option to whether it takes a value.
	globals map[string]bool
}

// commandFlags is one command's flag set. A value of true means the flag takes
// an argument.
type commandFlags struct {
	method string
	flags  map[string]bool
}

// docCommand is one `wlvision ...` line found in a skill document.
type docCommand struct {
	file string
	line int
	text string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// loadCLI derives the command table, the usage text, and the global flags from
// the CLI source. The dispatch is the source of truth for which commands exist;
// the usage text must agree with it.
func loadCLI(t *testing.T, dir string) *cliModel {
	t.Helper()

	model := &cliModel{commands: map[string]*commandFlags{}, globals: map[string]bool{}}

	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("cannot list %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	methods := map[string]*ast.FuncDecl{}
	functions := map[string]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("cannot parse %s: %v", path, err)
		}
		files[path] = file
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			functions[fn.Name.Name] = fn
			if name, ok := cliMethod(fn); ok {
				methods[name] = fn
			}
			if fn.Name.Name == "parseGlobals" {
				scanFlags(fn, model.globals)
			}
		}
	}

	if model.usage == "" {
		model.usage = findUsageText(t, files)
	}

	cliMethods := make(map[string]bool, len(methods))
	for name := range methods {
		cliMethods[name] = true
	}

	top := findSwitchOnIndex(functions["run"])
	if top == nil {
		t.Fatal("cmd/wlvision: the run dispatch switch was not found")
	}
	type group struct{ name, method string }
	var groups []group
	for _, clause := range switchCases(top) {
		name := caseString(clause)
		if name == "" {
			continue
		}
		method := firstMethodCall(clause.Body, cliMethods)
		if method == "" {
			t.Fatalf("cmd/wlvision: command %q has no dispatch call", name)
		}
		model.commands[name] = flagsOf(t, methods[method], method)
		groups = append(groups, group{name: name, method: method})
	}

	// A command may be a group: it dispatches on its first argument, the way
	// "session create" and "image build" do.
	for _, parent := range groups {
		subSwitch := findSwitchOnIndex(methods[parent.method])
		if subSwitch == nil {
			continue
		}
		for _, clause := range switchCases(subSwitch) {
			name := caseString(clause)
			if name == "" {
				continue
			}
			method := firstMethodCall(clause.Body, cliMethods)
			if method == "" {
				t.Fatalf("cmd/wlvision: %s subcommand %q has no dispatch call", parent.name, name)
			}
			model.commands[parent.name+" "+name] = flagsOf(t, methods[method], method)
		}
	}
	return model
}

// flagsOf returns the flag set a command method registers, including the flags
// its shared helpers register on its behalf.
func flagsOf(t *testing.T, fn *ast.FuncDecl, method string) *commandFlags {
	if fn == nil {
		t.Fatalf("cmd/wlvision: method %s was not found", method)
	}
	flags := map[string]bool{}
	scanFlags(fn, flags)
	return &commandFlags{method: method, flags: flags}
}

// scanFlags records every flag registered through a flag set named "flags" and
// the shared --session, --window, and --revision helpers. Values are the flag
// names mapped to whether the flag takes an argument.
func scanFlags(fn *ast.FuncDecl, flags map[string]bool) {
	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "flags" &&
				strings.HasSuffix(sel.Sel.Name, "Var") && len(node.Args) >= 2 {
				if lit, ok := node.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, err := strconv.Unquote(lit.Value)
					if err == nil && name != "" {
						flags[name] = sel.Sel.Name != "BoolVar"
					}
				}
			}
		case *ast.SelectorExpr:
			switch node.Sel.Name {
			case "sessionArgs":
				flags["session"] = true
			case "register":
				flags["window"] = true
				flags["revision"] = true
			}
		}
		return true
	})
}

// findUsageText evaluates the usageText constant. The expression is a chain of
// string literals and one non-literal element (the default image), which is
// rendered as a placeholder.
func findUsageText(t *testing.T, files map[string]*ast.File) string {
	t.Helper()
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if name.Name == "usageText" && i < len(vs.Values) {
						return evalString(vs.Values[i])
					}
				}
			}
		}
	}
	t.Fatal("cmd/wlvision: the usageText constant was not found")
	return ""
}

func evalString(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind == token.STRING {
			value, err := strconv.Unquote(node.Value)
			if err == nil {
				return value
			}
		}
	case *ast.BinaryExpr:
		if node.Op == token.ADD {
			return evalString(node.X) + evalString(node.Y)
		}
	case *ast.SelectorExpr, *ast.Ident:
		return "<dynamic>"
	}
	return ""
}

// findSwitchOnIndex returns the first switch in fn whose tag indexes a slice,
// which is how the CLI dispatches on a subcommand word.
func findSwitchOnIndex(fn *ast.FuncDecl) *ast.SwitchStmt {
	if fn == nil {
		return nil
	}
	var found *ast.SwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		if _, ok := sw.Tag.(*ast.IndexExpr); ok {
			found = sw
			return false
		}
		return true
	})
	return found
}

// switchCases returns the case clauses of a switch statement.
func switchCases(sw *ast.SwitchStmt) []*ast.CaseClause {
	var cases []*ast.CaseClause
	if sw == nil || sw.Body == nil {
		return cases
	}
	for _, stmt := range sw.Body.List {
		if clause, ok := stmt.(*ast.CaseClause); ok {
			cases = append(cases, clause)
		}
	}
	return cases
}

func caseString(clause *ast.CaseClause) string {
	if clause == nil || len(clause.List) != 1 {
		return ""
	}
	lit, ok := clause.List[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return value
}

// firstMethodCall returns the first call in body to one of the CLI's methods.
func firstMethodCall(body []ast.Stmt, methods map[string]bool) string {
	name := ""
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if name != "" {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && methods[sel.Sel.Name] {
				name = sel.Sel.Name
				return false
			}
			return true
		})
	}
	return name
}

func cliMethod(fn *ast.FuncDecl) (string, bool) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", false
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	ident, ok := typ.(*ast.Ident)
	if !ok || ident.Name != "cli" {
		return "", false
	}
	return fn.Name.Name, true
}

// extractCommands returns every `wlvision ...` line in a document: a fenced
// command inside a bash block, or an inline code span that begins with the
// command name.
func extractCommands(t *testing.T, path string) []docCommand {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var commands []docCommand
	inFence := false
	for index, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		record := func(text string) {
			commands = append(commands, docCommand{file: path, line: index + 1, text: text})
		}
		if inFence {
			if text := commandText(trimmed); text != "" {
				record(text)
			}
			continue
		}
		for _, span := range codeSpans(line) {
			if text := commandText(strings.TrimSpace(span)); text != "" {
				record(text)
			}
		}
	}
	return commands
}

// commandText returns the command when the text starts with the CLI binary.
func commandText(text string) string {
	if strings.HasPrefix(text, "wlvision ") || text == "wlvision" {
		return text
	}
	return ""
}

func codeSpans(line string) []string {
	parts := strings.Split(line, "`")
	var spans []string
	for i := 1; i < len(parts); i += 2 {
		spans = append(spans, parts[i])
	}
	return spans
}

// TestSkillCommandsMatchCLI checks every command line in the skill documents
// against the CLI: the command must be dispatched, every flag must belong to
// that command, and no command may mix mutually exclusive shapes.
func TestSkillCommandsMatchCLI(t *testing.T) {
	root := repoRoot(t)
	model := loadCLI(t, filepath.Join(root, "cmd", "wlvision"))

	commands, flagUses := 0, 0
	used := map[string]bool{}
	for _, document := range skillDocuments {
		path := filepath.Join(root, document)
		for _, command := range extractCommands(t, path) {
			key, uses := checkCommand(t, model, command)
			flagUses += uses
			commands++
			if key != "" {
				used[key] = true
			}
		}
	}
	if commands == 0 {
		t.Fatal("no wlvision commands were found in the skill documents")
	}
	t.Logf("checked %d commands and %d flag uses", commands, flagUses)

	checkUsageAgreesWithDispatch(t, model, used)
}

// checkCommand validates one documented command line and returns its command
// key and how many flags it used.
func checkCommand(t *testing.T, model *cliModel, command docCommand) (string, int) {
	t.Helper()
	fail := func(format string, args ...any) {
		args = append([]any{command.file, command.line, command.text}, args...)
		t.Errorf("%s:%d: command %q: "+format, args...)
	}

	tokens := strings.Fields(command.text)
	for len(tokens) > 0 && (tokens[len(tokens)-1] == "&" || tokens[len(tokens)-1] == "&&") {
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) == 0 || tokens[0] != "wlvision" {
		fail("does not start with the wlvision command")
		return "", 0
	}

	index := 1
	for index < len(tokens) {
		// A global flag is written with a dash: a bare word that happens to
		// share a flag's name is the command, not the flag.
		if !strings.HasPrefix(tokens[index], "-") {
			break
		}
		name, hasValue := flagToken(tokens[index])
		takesValue, ok := model.globals[name]
		if !ok {
			break
		}
		index++
		if takesValue && !hasValue {
			index++
		}
	}
	if index >= len(tokens) {
		fail("names no command")
		return "", 0
	}

	key := tokens[index]
	index++
	// A command may be a group: "session create", "image build". Join the next
	// token whenever that pair is a known command, so a group is read whole.
	if index < len(tokens) {
		if _, joined := model.commands[key+" "+tokens[index]]; joined {
			key += " " + tokens[index]
			index++
		}
	}
	if _, ok := model.commands[key]; !ok && key == "session" {
		fail("session names no subcommand")
		return "", 0
	}
	info, ok := model.commands[key]
	if !ok {
		fail("unknown command %q", key)
		return key, 0
	}

	var positionals []string
	flagUses := 0
	for index < len(tokens) {
		token := tokens[index]
		if token == "--" {
			positionals = append(positionals, tokens[index+1:]...)
			break
		}
		if strings.HasPrefix(token, "-") {
			name, hasValue := flagToken(token)
			takesValue, ok := info.flags[name]
			if !ok {
				fail("flag --%s does not belong to %q", name, key)
				index++
				continue
			}
			flagUses++
			index++
			if takesValue && !hasValue {
				index++
			}
			continue
		}
		positionals = append(positionals, token)
		index++
	}

	if len(positionals) > 0 && key != "type" && key != "run" {
		fail("unexpected positional argument %q", positionals[0])
	}
	if key == "run" {
		if !hasToken(tokens, "--") {
			fail("run requires -- before the application command")
		} else if lastToken(tokens) == "--" {
			fail("run requires an application command after --")
		}
	}
	checkExclusiveShapes(t, key, tokens, fail)
	return key, flagUses
}

// checkExclusiveShapes rejects a documented command that mixes the alternative
// shapes the CLI keeps apart.
func checkExclusiveShapes(t *testing.T, key string, tokens []string, fail func(string, ...any)) {
	t.Helper()
	switch key {
	case "inject":
		if hasToken(tokens, "--binary") == hasToken(tokens, "--bundle") {
			fail("inject takes exactly one of --binary or --bundle")
		}
	case "key":
		if hasToken(tokens, "--keycode") == hasToken(tokens, "--name") {
			fail("key takes exactly one of --keycode or --name")
		}
	case "pointer":
		groups := 0
		x, y := hasToken(tokens, "--x"), hasToken(tokens, "--y")
		if x || y {
			groups++
			if !x || !y {
				fail("pointer motion takes both --x and --y")
			}
		}
		button, state := hasToken(tokens, "--button"), hasToken(tokens, "--state")
		if button || state {
			groups++
			if !button || !state {
				fail("pointer button takes both --button and --state")
			}
		}
		axis, value := hasToken(tokens, "--axis"), hasToken(tokens, "--value")
		if axis || value {
			groups++
			if !axis || !value {
				fail("pointer axis takes both --axis and --value")
			}
		}
		if groups != 1 {
			fail("pointer takes exactly one of motion, button, or axis")
		}
	case "wait":
		predicates := 0
		for _, flag := range []string{"--stable-for", "--window-count", "--process-exit", "--new-frame", "--window"} {
			if hasToken(tokens, flag) {
				predicates++
			}
		}
		if predicates != 1 {
			fail("wait takes exactly one predicate")
		}
		if !hasToken(tokens, "--window") &&
			(hasToken(tokens, "--width") || hasToken(tokens, "--height") || hasToken(tokens, "--state")) {
			fail("wait --width, --height and --state require --window")
		}
	case "type":
		if hasToken(tokens, "--text") && positionalToken(tokens) {
			fail("type does not mix --text with a positional text")
		}
	}
}

// checkUsageAgreesWithDispatch proves that every command the skill documents is
// named by the usage text, and that the usage text names no command the CLI does
// not dispatch, so the two describe the same surface.
func checkUsageAgreesWithDispatch(t *testing.T, model *cliModel, used map[string]bool) {
	t.Helper()
	for key := range used {
		if !strings.Contains(model.usage, "\n  "+key) {
			t.Errorf("cmd/wlvision: the skill documents %q, which the usage text does not name", key)
		}
	}
	dispatched := map[string]bool{}
	for key := range model.commands {
		dispatched[strings.Fields(key)[0]] = true
	}
	line := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*)\b`)
	for _, match := range line.FindAllStringSubmatch(model.usage, -1) {
		if !dispatched[match[1]] {
			t.Errorf("cmd/wlvision: usageText names %q, which is not a dispatched command", match[1])
		}
	}
}

// TestSkillContractReferencesExist proves that the codes, envelope keys, and
// JSON fields the skill documents name exist in the contract sources.
func TestSkillContractReferencesExist(t *testing.T) {
	root := repoRoot(t)

	codes := parseCodes(t, filepath.Join(root, "internal", "result", "codes.go"))
	keys := parseEnvelopeKeys(t, filepath.Join(root, "internal", "result", "envelope.go"))
	allowed := parseContractIdentifiers(t, root)

	requiredCodes := []string{
		"engine_unavailable", "engine_not_rootless", "protection_degraded",
		"image_unavailable", "payload_rejected", "session_not_ready",
		"window_not_found", "stale_revision", "capture_failed", "wait_timeout",
		"process_exited", "weston_protocol_mismatch", "usage_error",
	}
	for _, code := range requiredCodes {
		if !codes[code] {
			t.Errorf("internal/result/codes.go does not declare the code %q", code)
		}
	}
	requiredKeys := []string{"schema", "ok", "operation", "session", "revision", "result", "error", "warnings"}
	for _, key := range requiredKeys {
		if !keys[key] {
			t.Errorf("internal/result/envelope.go does not declare the envelope key %q", key)
		}
	}
	for code := range codes {
		allowed[code] = true
	}

	for _, document := range skillDocuments {
		path := filepath.Join(root, document)
		for lineIndex, line := range readLines(t, path) {
			for _, span := range codeSpans(line) {
				for _, name := range identifierComponents(strings.TrimSpace(span)) {
					if !allowed[name] {
						t.Errorf("%s:%d: %q names %q, which is not a code, envelope key, or JSON field of the contract",
							path, lineIndex+1, strings.TrimSpace(span), name)
					}
				}
			}
		}
	}
}

// identifierComponents returns the lowercase dotted components of a code span
// that is entirely identifier-like, and nothing for any other span.
func identifierComponents(span string) []string {
	if span == "" {
		return nil
	}
	if !identifierLike.MatchString(span) {
		return nil
	}
	if !strings.ContainsAny(span, "_.") {
		return nil
	}
	return strings.Split(span, ".")
}

var identifierLike = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)*$`)

func parseCodes(t *testing.T, path string) map[string]bool {
	t.Helper()
	codes := map[string]bool{}
	pattern := regexp.MustCompile(`Code\s*=\s*"([a-z_]+)"`)
	for _, match := range pattern.FindAllStringSubmatch(readFile(t, path), -1) {
		codes[match[1]] = true
	}
	if len(codes) == 0 {
		t.Fatalf("%s declares no error codes", path)
	}
	return codes
}

func parseEnvelopeKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	pattern := regexp.MustCompile(`json:"([a-z][a-z0-9_]*)`)
	for _, match := range pattern.FindAllStringSubmatch(readFile(t, path), -1) {
		keys[match[1]] = true
	}
	if len(keys) == 0 {
		t.Fatalf("%s declares no JSON keys", path)
	}
	return keys
}

// parseContractIdentifiers collects every JSON field name and stable string
// value declared by the contract packages, so a document may name only what the
// contract defines.
func parseContractIdentifiers(t *testing.T, root string) map[string]bool {
	t.Helper()
	paths := []string{
		filepath.Join("internal", "result", "envelope.go"),
		filepath.Join("internal", "result", "codes.go"),
		filepath.Join("internal", "agentapi", "wire.go"),
		filepath.Join("internal", "session", "model.go"),
		filepath.Join("internal", "session", "service.go"),
		filepath.Join("internal", "engine", "engine.go"),
		filepath.Join("internal", "capture", "vision.go"),
		filepath.Join("cmd", "wlvision", "main.go"),
		filepath.Join("cmd", "wlvision", "interaction.go"),
		filepath.Join("cmd", "wlvision", "capture.go"),
		filepath.Join("cmd", "wlvision", "wait.go"),
	}
	identifiers := map[string]bool{}
	jsonKey := regexp.MustCompile(`json:"([a-z][a-z0-9_]*)`)
	value := regexp.MustCompile(`= "([a-z][a-z0-9_]*)"`)
	for _, relative := range paths {
		content := readFile(t, filepath.Join(root, relative))
		for _, match := range jsonKey.FindAllStringSubmatch(content, -1) {
			identifiers[match[1]] = true
		}
		for _, match := range value.FindAllStringSubmatch(content, -1) {
			identifiers[match[1]] = true
		}
	}
	return identifiers
}

// TestSkillDocumentsUsePlaceholders rejects a document that names a real home
// path, an email address, or a session that is not a placeholder.
func TestSkillDocumentsUsePlaceholders(t *testing.T) {
	root := repoRoot(t)
	homePath := regexp.MustCompile(`/(?:home|Users)/([A-Za-z0-9._-]+)`)
	email := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	sessionFlag := regexp.MustCompile(`--session\s+(\S+)`)
	stateRootFlag := regexp.MustCompile(`--state-root\s+(\S+)`)

	for _, document := range markdownFiles(t, root) {
		for lineIndex, line := range readLines(t, document) {
			if match := homePath.FindStringSubmatch(line); match != nil && !placeholderValue(match[1]) && match[1] != "agent" {
				t.Errorf("%s:%d: %q names the host path %q", document, lineIndex+1, line, match[0])
			}
			if match := email.FindString(line); match != "" {
				t.Errorf("%s:%d: %q names the address %q", document, lineIndex+1, line, match)
			}
			if !strings.Contains(line, "wlvision ") {
				continue
			}
			if match := sessionFlag.FindStringSubmatch(line); match != nil && !placeholderValue(match[1]) {
				t.Errorf("%s:%d: %q uses the session %q, which is not a placeholder", document, lineIndex+1, line, match[1])
			}
			if match := stateRootFlag.FindStringSubmatch(line); match != nil && !placeholderValue(match[1]) {
				t.Errorf("%s:%d: %q uses the state root %q, which is not a placeholder", document, lineIndex+1, line, match[1])
			}
		}
	}
}

// placeholderValue reports whether a flag value is a documented placeholder
// rather than a real name or path.
func placeholderValue(value string) bool {
	return strings.HasPrefix(value, "<") || strings.HasPrefix(value, "$") ||
		strings.HasPrefix(value, "{") || value == ""
}

// TestSkillFencesHoldOnlyCommands rejects a skill document whose code fences are
// not bash blocks of wlvision commands, which is what keeps implementation
// detail out of the skill.
func TestSkillFencesHoldOnlyCommands(t *testing.T) {
	root := repoRoot(t)
	for _, document := range skillDocuments {
		path := filepath.Join(root, document)
		inFence, fenceInfo, fences := false, "", 0
		for lineIndex, line := range readLines(t, path) {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				if !inFence {
					inFence = true
					fenceInfo = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
					fences++
					if fenceInfo != "bash" {
						t.Errorf("%s:%d: code fence is %q, want bash", path, lineIndex+1, fenceInfo)
					}
				} else {
					inFence = false
				}
				continue
			}
			if inFence && trimmed != "" && !strings.HasPrefix(trimmed, "wlvision ") {
				t.Errorf("%s:%d: %q is not a wlvision command", path, lineIndex+1, trimmed)
			}
		}
		if fences == 0 {
			t.Errorf("%s holds no command block", path)
		}
	}
}

// markdownFiles returns every Markdown file under skills/ and docs/.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{filepath.Join(root, "skills"), filepath.Join(root, "docs")} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && strings.HasSuffix(path, ".md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("cannot walk %s: %v", dir, err)
		}
	}
	sort.Strings(files)
	return files
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(data)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(readFile(t, path), "\n")
}

// flagToken splits a flag token into its name and whether it carried a value.
func flagToken(token string) (string, bool) {
	trimmed := strings.TrimLeft(token, "-")
	if index := strings.IndexByte(trimmed, '='); index >= 0 {
		return trimmed[:index], true
	}
	return trimmed, false
}

func hasToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want || strings.HasPrefix(token, want+"=") {
			return true
		}
	}
	return false
}

func lastToken(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	return tokens[len(tokens)-1]
}

// positionalToken reports whether a command line carries a positional argument
// that is not a flag value.
func positionalToken(tokens []string) bool {
	skipNext := false
	for index, token := range tokens {
		if index < 2 {
			continue
		}
		if skipNext {
			skipNext = false
			continue
		}
		if token == "--" {
			return index+1 < len(tokens)
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		return true
	}
	return false
}

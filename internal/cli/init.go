package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nchapman/vamos/internal/config"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/lifecycle"
	"github.com/nchapman/vamos/internal/registry"
	"github.com/nchapman/vamos/internal/ssh"
	"go.yaml.in/yaml/v3"
)

// initArguments moves only positional tokens, never flag values. In particular a
// value that resembles an option remains attached to its option for validation.
func initArguments(args []string) ([]string, error) {
	var options, positional []string
	values := map[string]bool{"port": true, "f": true, "server": true, "service": true, "health": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, _, equals := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !values[name] && name != "private" && name != "h" && name != "help" {
			return nil, fmt.Errorf("unknown init flag %s", arg)
		}
		options = append(options, arg)
		if values[name] && !equals {
			i++
			if i == len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			options = append(options, args[i])
		}
	}
	return append(append(options, "--"), positional...), nil
}

func initCommand(args []string, stdout, stderr io.Writer, registryFn func() (*registry.Store, error)) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	filename := flags.String("f", config.DefaultFile, "output config (must not exist)")
	port := flags.Int("port", 0, "explicit public HTTP port")
	serverName := flags.String("server", "", "registered server")
	service := flags.String("service", "web", "service name")
	health := flags.String("health", "/", "HTTP health path")
	private := flags.Bool("private", false, "private service without HTTP")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: vamos init APP --port PORT [-f vamos.yaml] [--server NAME] [--service NAME] [--health PATH]\n       vamos init APP --private [-f vamos.yaml] [--server NAME] [--service NAME]")
	}
	normalized, err := initArguments(args)
	if err != nil {
		return usageError(stderr, err.Error())
	}
	if err := parseFlags(flags, normalized, stdout, stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	provided := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if flags.NArg() != 1 || *filename == "" || (provided["server"] && *serverName == "") {
		return usageError(stderr, "init requires one APP and nonempty output/server options")
	}
	if (*private && (provided["port"] || provided["health"])) || (!*private && !provided["port"]) {
		return usageError(stderr, "public init requires --port; --private cannot be combined with --port or --health")
	}
	svc := config.Service{Build: &config.Build{Context: ".", Dockerfile: "Dockerfile"}}
	if !*private {
		svc.HTTP = &config.HTTP{Port: *port, Health: *health}
	}
	app := &config.App{Name: flags.Arg(0), Services: map[string]config.Service{*service: svc}}
	data, err := yaml.Marshal(app)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	validated, err := config.Decode(bytes.NewReader(data))
	if err != nil {
		return usageError(stderr, err.Error())
	}
	if err := deploy.ValidateApp(validated); err != nil {
		return usageError(stderr, err.Error())
	}
	path, err := filepath.Abs(*filename)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	dir := filepath.Dir(path)
	if filepath.Base(path) == ".vamos" {
		return lifecycleError(stderr, errors.New("output conflicts with .vamos metadata"))
	}
	for p := dir; filepath.Dir(p) != p; p = filepath.Dir(p) {
		if filepath.Base(p) == ".vamos" {
			return lifecycleError(stderr, errors.New("output must not be inside .vamos metadata"))
		}
	}
	// Never create output parents or follow a symlink in the selected path.
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if !info.IsDir() {
			return lifecycleError(stderr, errors.New("output parents must be directories, not symlinks"))
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return lifecycleError(stderr, errors.New("output already exists; init never overwrites"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return lifecycleError(stderr, err)
	}
	store, err := registryFn()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	state, err := store.Read()
	if err != nil {
		return lifecycleError(stderr, err)
	}
	var server registry.Server
	if *serverName != "" || state.Default != "" {
		server, err = selectAppServer(state, "", "", *serverName)
		if err != nil {
			return lifecycleError(stderr, err)
		}
		if err := lifecycle.ValidateConfig(lifecycle.Config{Name: server.Name, Domain: server.Domain}); err != nil {
			return lifecycleError(stderr, err)
		}
		if err := ssh.ValidateTarget(server.Target); err != nil {
			return lifecycleError(stderr, err)
		}
	}
	if err := initMetadataPreflight(dir, app.Name, server); err != nil {
		return lifecycleError(stderr, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return lifecycleError(stderr, err)
	}
	partial := func(err error) int {
		return lifecycleError(stderr, fmt.Errorf("init partially wrote %s; configuration or .vamos metadata may remain; inspect before retrying (nothing removed): %w", path, err))
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return partial(err)
	}
	if server.Name != "" {
		// bindProject does not re-check ignore metadata for an already equal binding.
		// Protect it here too, without replacing any existing file.
		root := filepath.Join(dir, ".vamos")
		if err := os.MkdirAll(root, 0700); err != nil {
			return partial(err)
		}
		if err := initMetadataPreflight(dir, app.Name, server); err != nil {
			return partial(err)
		}
		ignore, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, e := ignore.WriteString("*\n")
			err = errors.Join(e, ignore.Close())
		} else if errors.Is(err, os.ErrExist) {
			err = nil
		}
		if err != nil {
			return partial(err)
		}
		if err := bindProject(dir, app.Name, server); err != nil {
			return partial(err)
		}
	}
	bindingMessage := "unbound (no registered servers)"
	if server.Name != "" {
		bindingMessage = "bound to " + server.Name
	}
	if err := writeInitSuccess(stdout, app.Name, *filename, bindingMessage); err != nil {
		return partial(err)
	}
	return 0
}

func initMetadataPreflight(dir, app string, server registry.Server) error {
	current, err := readBinding(dir)
	if err != nil {
		return err
	}
	if current != nil && *current != (binding{Version: 1, App: app, Server: server.Name, Target: server.Target}) {
		return errors.New("existing .vamos binding differs from requested app/server")
	}
	root := filepath.Join(dir, ".vamos")
	for _, name := range []string{"binding.lock", ".gitignore"} {
		if err := safeMetadata(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	data, err := readProjectMetadata(filepath.Join(root, ".gitignore"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(data)) != "*" {
		return errors.New("existing .vamos/.gitignore must contain '*' to protect project metadata")
	}
	return nil
}

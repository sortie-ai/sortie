package agenttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"
)

//go:embed testdata/sshlab
var sshLabFiles embed.FS

const (
	sshLabUser       = "sortie"
	sshLabHost       = "sortie-lab"
	sshLabRefusedKey = "sortie-lab-refused"

	// sshLabBuildBound covers a cold image build.
	sshLabBuildBound = 20 * time.Minute
)

// SSHLabParams describes one lab for [StartSSHLab].
type SSHLabParams struct {
	// Install is a POSIX shell script that runs as root while the image
	// builds and puts the kind's runtime on the login user's PATH. The image
	// has Node.js, git, curl and unzip. Docker caches the result per script.
	Install string

	// Dirs are absolute paths created empty and owned by the login user.
	Dirs []string

	// Seed lists host directories copied to the same path in the container.
	// Nothing else of the host reaches the container and nothing is mounted.
	Seed []string
}

// SSHLab is a running throwaway sshd container.
type SSHLab struct {
	// Host is the ssh destination of an sshd that allows TCP forwarding.
	Host string
	// RefusedHost is the ssh destination of a second sshd in the same
	// container with AllowTcpForwarding no.
	RefusedHost string

	container string
}

// Processes returns the command line of every process the login user runs on
// the lab host, leaving out the sshd sessions that serve its connections.
func (l SSHLab) Processes(ctx context.Context) ([]string, error) {
	procs, err := l.loginProcesses(ctx)
	if err != nil {
		return nil, err
	}
	args := make([]string, len(procs))
	for i, proc := range procs {
		args[i] = proc.args
	}
	return args, nil
}

// Kill sends SIGKILL, from inside the lab container, to every process
// [SSHLab.Processes] lists, and returns once the signal is sent. The signal
// does not go through ssh, so the remote runtime dies the way a local one does
// and sshd closes the channel on its own.
func (l SSHLab) Kill(ctx context.Context) error {
	procs, err := l.loginProcesses(ctx)
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		return nil
	}
	// kill may be only a shell builtin in the image, so the signal goes through
	// sh. A listed process can exit before its signal; only a failure that
	// leaves the process alive is an error.
	const script = `for p in "$@"; do kill -KILL "$p" 2>/dev/null || ! kill -0 "$p" 2>/dev/null || exit 1; done`
	args := []string{"exec", l.container, "sh", "-c", script, "sh"}
	for _, proc := range procs {
		args = append(args, strconv.Itoa(proc.pid))
	}
	if out, err := dockerOutput(ctx, args...); err != nil {
		return fmt.Errorf("kill the processes of SSH lab container %s: %w\n%s", l.container, err, tail(out))
	}
	return nil
}

type labProcess struct {
	pid  int
	args string
}

func (l SSHLab) loginProcesses(ctx context.Context) ([]labProcess, error) {
	out, err := dockerOutput(ctx, "exec", l.container, "ps", "-e", "-o", "user=,pid=,args=")
	if err != nil {
		return nil, fmt.Errorf("list the processes of SSH lab container %s: %w\n%s", l.container, err, tail(out))
	}
	var procs []labProcess
	for line := range strings.Lines(string(out)) {
		user, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		pidField, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
		pid, err := strconv.Atoi(pidField)
		args = strings.TrimSpace(args)
		if err != nil || user != sshLabUser || strings.HasPrefix(args, "sshd") {
			continue
		}
		procs = append(procs, labProcess{pid: pid, args: args})
	}
	return procs, nil
}

// SSHLabSkipReason returns why the lab cannot run on this host, or "" when it
// can. It names docker when docker is the missing tool.
func SSHLabSkipReason() string {
	if runtime.GOOS == "windows" {
		return "the SSH lab needs a unix host: its ssh wrapper is a shell script"
	}
	for _, tool := range []string{"docker", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Sprintf("the SSH lab needs %s on PATH", tool)
		}
	}
	return ""
}

// StartSSHLab builds the lab image, starts a container with --network host and
// returns once both sshds listen on free loopback ports.
//
// The ssh found through PATH becomes a wrapper that reads only a generated
// configuration, so ~/.ssh/config cannot redirect a launch. StartSSHLab
// therefore sets PATH for the rest of the test and must not be called from a
// parallel test. The container is removed in t.Cleanup.
//
// It fails t when [SSHLabSkipReason] is not empty; callers skip first.
func StartSSHLab(t testing.TB, p SSHLabParams) SSHLab {
	t.Helper()

	if reason := SSHLabSkipReason(); reason != "" {
		t.Fatalf("StartSSHLab: %s", reason)
	}
	dirs, err := labDirs(p)
	if err != nil {
		t.Fatalf("StartSSHLab: %v", err)
	}

	image := buildSSHLabImage(t, p.Install)
	keyPath, publicKey := newLabKey(t)
	port, refusedPort := freeLoopbackPort(t), freeLoopbackPort(t)
	name := "sortie-sshlab-" + randomLabID(t)

	runLabContainer(t, name, image, labContainerEnv{
		publicKey:   publicKey,
		dirs:        dirs,
		port:        port,
		refusedPort: refusedPort,
	})
	for _, dir := range p.Seed {
		seedLabDir(t, name, dir)
	}
	installLabSSH(t, keyPath, port, refusedPort)
	return SSHLab{Host: sshLabHost, RefusedHost: sshLabRefusedKey, container: name}
}

func labDirs(p SSHLabParams) ([]string, error) {
	var dirs []string
	for _, dir := range slices.Concat(p.Dirs, p.Seed) {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("directory %q is not absolute", dir)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func buildSSHLabImage(t testing.TB, install string) string {
	t.Helper()

	files := map[string][]byte{"install.sh": []byte("set -eu\n" + install + "\n")}
	for _, name := range []string{"Dockerfile", "sshd_config", "entrypoint.sh"} {
		data, err := sshLabFiles.ReadFile("testdata/sshlab/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		files[name] = data
	}

	sum := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		sum.Write([]byte(name))
		sum.Write(files[name])
	}
	image := "sortie-agenttest-sshlab:" + hex.EncodeToString(sum.Sum(nil))[:16]

	dir := t.TempDir()
	for name, data := range files {
		mode := os.FileMode(0o644)
		if name == "entrypoint.sh" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil { //nolint:gosec // G306: the lab's own build context under t.TempDir; the entrypoint must be executable.
			t.Fatalf("write build context %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), sshLabBuildBound)
	defer cancel()
	if out, err := dockerOutput(ctx, "build", "--tag", image, dir); err != nil {
		t.Fatalf("docker build %s: %v\n%s", image, err, tail(out))
	}
	return image
}

func newLabKey(t testing.TB) (path, public string) {
	t.Helper()

	path = filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.CommandContext(waittest.Context(t), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sortie-sshlab", "-f", path).CombinedOutput(); err != nil { //nolint:gosec // G204: the key path is under t.TempDir.
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path + ".pub") //nolint:gosec // G304: the key was just written under t.TempDir.
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	return path, strings.TrimSpace(string(data))
}

func freeLoopbackPort(t testing.TB) int {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer ln.Close() //nolint:errcheck // the listener carried no traffic, so a close error loses nothing
	return ln.Addr().(*net.TCPAddr).Port
}

func randomLabID(t testing.TB) string {
	t.Helper()

	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(raw)
}

type labContainerEnv struct {
	publicKey   string
	dirs        []string
	port        int
	refusedPort int
}

func runLabContainer(t testing.TB, name, image string, env labContainerEnv) {
	t.Helper()

	logs := &labLog{ready: make(chan struct{}), markers: []string{
		"Server listening on 127.0.0.1 port " + strconv.Itoa(env.port) + ".",
		"Server listening on 127.0.0.1 port " + strconv.Itoa(env.refusedPort) + ".",
	}}
	cmd := exec.CommandContext(context.Background(), "docker", "run", "--rm", "--init", "--network", "host", "--name", name, //nolint:gosec // G204: the arguments are the lab's own.
		"-e", "SORTIE_LAB_AUTHORIZED_KEY="+env.publicKey,
		"-e", "SORTIE_LAB_DIRS="+strings.Join(env.dirs, "\n"),
		"-e", "SORTIE_LAB_PORT="+strconv.Itoa(env.port),
		"-e", "SORTIE_LAB_REFUSED_PORT="+strconv.Itoa(env.refusedPort),
		image)
	cmd.Env = dockerEnv()
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("docker run %s: %v", image, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("SSH lab container %s output:\n%s", name, tail(logs.snapshot()))
		}
		removeCtx := waittest.CleanupContext(t)
		if out, err := dockerOutput(removeCtx, "rm", "--force", name); err != nil {
			t.Logf("docker rm --force %s: %v\n%s", name, err, out)
			_ = cmd.Process.Kill()
		}
		if _, ok := waittest.ReceiveWithin(exited, waittest.HangGuard); !ok {
			t.Errorf("docker run for %s still running %v after the container was removed", name, waittest.HangGuard)
		}
	})

	ctx := waittest.Context(t)
	select {
	case <-logs.ready:
	case err := <-exited:
		t.Fatalf("SSH lab container %s exited before its sshds listened: %v\n%s", name, err, tail(logs.snapshot()))
	case <-ctx.Done():
		t.Fatalf("SSH lab container %s: sshds not listening within %v\n%s", name, waittest.HangGuard, tail(logs.snapshot()))
	}
}

func seedLabDir(t testing.TB, container, dir string) {
	t.Helper()

	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	ctx := waittest.Context(t)
	if out, err := dockerOutput(ctx, "cp", dir+string(filepath.Separator)+".", container+":"+dir); err != nil {
		t.Fatalf("docker cp %s: %v\n%s", dir, err, out)
	}
	if out, err := dockerOutput(ctx, "exec", container, "chown", "-R", sshLabUser+":"+sshLabUser, "--", dir); err != nil {
		t.Fatalf("chown seeded %s: %v\n%s", dir, err, out)
	}
}

func installLabSSH(t testing.TB, keyPath string, port, refusedPort int) {
	t.Helper()

	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatalf("find ssh: %v", err)
	}
	dir := t.TempDir()
	host := func(alias string, port int) string {
		return fmt.Sprintf("Host %s\n  HostName 127.0.0.1\n  Port %d\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  PasswordAuthentication no\n  UserKnownHostsFile %s\n  GlobalKnownHostsFile /dev/null\n",
			alias, port, sshLabUser, keyPath, filepath.Join(dir, "known_hosts"))
	}
	config := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(config, []byte(host(sshLabHost, port)+host(sshLabRefusedKey, refusedPort)), 0o600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create %s: %v", binDir, err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s -F %s \"$@\"\n", shellSingleQuote(realSSH), shellSingleQuote(config))
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the wrapper must be executable.
		t.Fatalf("write ssh wrapper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dockerEnv restores the account's own home: a test points HOME at an isolated
// directory, and docker finds its plugins and contexts under the real one.
func dockerEnv() []string {
	env := os.Environ()
	if account, err := user.Current(); err == nil && account.HomeDir != "" {
		env = append(env, "HOME="+account.HomeDir)
	}
	return env
}

func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: the arguments are the lab's own.
	cmd.Env = dockerEnv()
	return cmd.CombinedOutput()
}

type labLog struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	markers []string
	ready   chan struct{}
	closed  bool
}

func (l *labLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	if !l.closed && !slices.ContainsFunc(l.markers, func(m string) bool { return !bytes.Contains(l.buf.Bytes(), []byte(m)) }) {
		l.closed = true
		close(l.ready)
	}
	return len(p), nil
}

func (l *labLog) snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.buf.Bytes())
}

func tail(out []byte) string {
	const keep = 4000
	if len(out) > keep {
		out = out[len(out)-keep:]
	}
	return string(out)
}

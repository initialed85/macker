// This file implements Macker's local OCI image and workload operations.
// It deliberately uses only the Go standard library.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	layoutVersion       = "1.0.0"
	manifestMediaType   = "application/vnd.oci.image.manifest.v1+json"
	configMediaType     = "application/vnd.oci.image.config.v1+json"
	layerMediaType      = "application/vnd.oci.image.layer.v1.tar"
	gzipLayerMediaType  = "application/vnd.oci.image.layer.v1.tar+gzip"
	defaultImageCommand = "/bin/sh"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *platform         `json:"platform,omitempty"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []descriptor `json:"manifests"`
}

type imageLayout struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

type runtimeConfig struct {
	User       string   `json:"User,omitempty"`
	Env        []string `json:"Env,omitempty"`
	Entrypoint []string `json:"Entrypoint,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type historyEntry struct {
	Created   string `json:"created,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

type imageConfig struct {
	Created      string         `json:"created,omitempty"`
	Architecture string         `json:"architecture"`
	OS           string         `json:"os"`
	Config       runtimeConfig  `json:"config"`
	RootFS       rootFS         `json:"rootfs"`
	History      []historyEntry `json:"history,omitempty"`
}

type loadedImage struct {
	Layout    string
	Reference string
	Manifest  manifest
	Config    imageConfig
}

type buildOptions struct {
	RootFS       string
	Output       string
	Tag          string
	Architecture string
	Entrypoint   string
	Args         []string
	Env          []string
	WorkingDir   string
}

func commandOCI(args []string) error {
	if len(args) == 0 {
		return errors.New("oci requires a command: build, inspect, unpack, or run")
	}
	switch args[0] {
	case "build":
		return commandOCIBuild(args[1:])
	case "inspect":
		return commandOCIInspect(args[1:])
	case "unpack":
		return commandOCIUnpack(args[1:])
	case "run":
		return commandOCIRun(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stderr, `Usage:
  macker oci build   [flags]
  macker oci inspect [flags] IMAGE
  macker oci unpack  [flags] IMAGE
  macker oci run     [flags] IMAGE [-- COMMAND ARG...]

--tty attaches the caller's terminal; --interactive keeps standard input
attached.`)
		return nil
	default:
		return fmt.Errorf("unknown oci command %q", args[0])
	}
}

func commandOCIBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rootfs := fs.String("rootfs", "", "directory to package as the image root filesystem")
	output := fs.String("output", "", "OCI image layout directory to create")
	tag := fs.String("tag", "darwin-workload:latest", "OCI layout reference name")
	arch := fs.String("architecture", runtime.GOARCH, "Darwin architecture recorded in the image")
	entrypoint := fs.String("entrypoint", "", "absolute executable path inside the image")
	var imageArgs stringList
	var env stringList
	fs.Var(&imageArgs, "arg", "argument appended to the image command (repeatable)")
	fs.Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
	workingDir := fs.String("workdir", "/", "working directory inside the image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("build does not accept positional arguments")
	}
	if *rootfs == "" || *output == "" || *entrypoint == "" {
		return errors.New("--rootfs, --output, and --entrypoint are required")
	}
	if *arch == "" {
		return errors.New("--architecture must not be empty")
	}
	if !strings.HasPrefix(*entrypoint, "/") {
		return errors.New("--entrypoint must be an absolute path inside the image")
	}
	if !strings.HasPrefix(*workingDir, "/") {
		return errors.New("--workdir must be an absolute path inside the image")
	}

	return buildImage(buildOptions{
		RootFS:       *rootfs,
		Output:       *output,
		Tag:          *tag,
		Architecture: *arch,
		Entrypoint:   *entrypoint,
		Args:         imageArgs,
		Env:          env,
		WorkingDir:   *workingDir,
	})
}

func commandOCIInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "OCI layout reference name; defaults to the first manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("inspect requires exactly one OCI layout directory")
	}
	img, err := loadImage(fs.Arg(0), *tag)
	if err != nil {
		return err
	}

	fmt.Printf("layout:      %s\n", img.Layout)
	fmt.Printf("reference:   %s\n", img.Reference)
	fmt.Printf("platform:    %s/%s\n", img.Config.OS, img.Config.Architecture)
	fmt.Printf("entrypoint:  %s\n", strings.Join(img.Config.Config.Entrypoint, " "))
	fmt.Printf("cmd:         %s\n", strings.Join(img.Config.Config.Cmd, " "))
	fmt.Printf("workdir:     %s\n", img.Config.Config.WorkingDir)
	fmt.Printf("layers:      %d\n", len(img.Manifest.Layers))
	for i, layer := range img.Manifest.Layers {
		fmt.Printf("  %d: %s %d bytes\n", i, layer.Digest, layer.Size)
	}
	return nil
}

func commandOCIUnpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	output := fs.String("output", "", "directory to create for the unpacked root filesystem")
	tag := fs.String("tag", "", "OCI layout reference name; defaults to the first manifest")
	force := fs.Bool("force", false, "remove an existing output directory first")
	allowMismatch := fs.Bool("allow-platform-mismatch", false, "allow unpacking a non-Darwin or non-native architecture image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("unpack requires exactly one OCI layout directory")
	}
	if *output == "" {
		return errors.New("--output is required")
	}
	img, err := loadImage(fs.Arg(0), *tag)
	if err != nil {
		return err
	}
	if !*allowMismatch {
		if err := checkPlatform(img.Config); err != nil {
			return err
		}
	}
	return unpackImage(img, *output, *force)
}

func commandOCIRun(args []string) error {
	args = expandInteractiveShortFlags(args)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	tag := fs.String("tag", "", "OCI layout reference name; defaults to the first manifest")
	rootfs := fs.String("rootfs", "", "extract into this directory instead of a temporary directory")
	chroot := fs.Bool("chroot", false, "run with the extracted directory as the process root; requires root on macOS")
	skipUnpack := fs.Bool("skip-unpack", false, "use --rootfs as an already-unpacked rootfs")
	allowMismatch := fs.Bool("allow-platform-mismatch", false, "allow running a non-Darwin or non-native architecture image")
	interactive := fs.Bool("i", false, "keep standard input open")
	tty := fs.Bool("t", false, "attach the caller's terminal")
	hostFallback := fs.Bool("host-fallback", false, "allow an image command to fall back to a host executable")
	pidFile := fs.String("pid-file", "", "write the workload PID to this internal file")
	entrypoint := fs.String("entrypoint", "", "override the image entrypoint")
	statusFile := fs.String("status-file", "", "write internal workload exit information to this file")
	fs.BoolVar(interactive, "interactive", false, "keep standard input open")
	fs.BoolVar(tty, "tty", false, "attach the caller's terminal")
	var env stringList
	var commandArgs stringList
	fs.Var(&env, "env", "environment variable KEY=VALUE override (repeatable)")
	fs.Var(&commandArgs, "arg", "argument appended to the image command (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("run requires an OCI layout directory")
	}
	imagePath := fs.Arg(0)
	commandOverride := fs.Args()[1:]
	if len(commandOverride) > 0 && commandOverride[0] == "--" {
		commandOverride = commandOverride[1:]
	}

	img, err := loadImage(imagePath, *tag)
	if err != nil {
		return err
	}
	if !*allowMismatch {
		if err := checkPlatform(img.Config); err != nil {
			return err
		}
	}

	extractDir := *rootfs
	ownedRootFS := false
	if *tty && *chroot {
		return errors.New("--tty cannot be combined with --chroot")
	}
	if *skipUnpack {
		if extractDir == "" {
			return errors.New("--skip-unpack requires --rootfs")
		}
		info, statErr := os.Lstat(extractDir)
		if statErr != nil {
			return fmt.Errorf("stat existing rootfs: %w", statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("existing rootfs %q is not a normal directory", extractDir)
		}
	} else if extractDir == "" {
		extractDir, err = os.MkdirTemp("", "macker-rootfs-")
		if err != nil {
			return fmt.Errorf("create temporary rootfs: %w", err)
		}
		ownedRootFS = true
	} else if err := prepareOutputDir(extractDir, false); err != nil {
		return err
	}
	if ownedRootFS {
		defer os.RemoveAll(extractDir)
	}
	if !*skipUnpack {
		if err := unpackImage(img, extractDir, false); err != nil {
			return err
		}
	}

	command := selectImageCommand(*entrypoint, img.Config.Config.Entrypoint, img.Config.Config.Cmd, commandArgs, commandOverride)

	workdir := img.Config.Config.WorkingDir
	if workdir == "" {
		workdir = "/"
	}
	if !strings.HasPrefix(workdir, "/") {
		return fmt.Errorf("image working directory %q is not absolute", workdir)
	}

	envVars := mergeEnvironment(img.Config.Config.Env, env)
	if *tty {
		envVars = ensureTerminalEnvironment(envVars)
	}
	if replacedFiles, err := substituteMackerConfig(extractDir, mackerEnvironmentValues(envVars)); err != nil {
		return err
	} else if replacedFiles > 0 {
		fmt.Fprintf(os.Stderr, "substituted Macker tokens in %d config file(s)\n", replacedFiles)
	}
	imageCommand, err := resolveImageCommand(extractDir, command[0], workdir, envVars)
	if err != nil && ((*entrypoint == "" && !*hostFallback) || *chroot) {
		return err
	}

	var cmd *exec.Cmd
	if *chroot {
		cmd = exec.Command(imageCommand, command[1:]...)
		cmd.Dir = workdir
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: extractDir, Setpgid: true}
	} else {
		hostCommand := filepath.Join(extractDir, filepath.FromSlash(strings.TrimPrefix(imageCommand, "/")))
		usingHostEntrypoint := false
		if (*entrypoint != "" || *hostFallback) && (err != nil || !isExecutableFile(hostCommand)) {
			hostCommand, err = resolveHostCommand(command[0])
			if err != nil {
				return fmt.Errorf("resolve command %q in image or on host: %w", command[0], err)
			}
			usingHostEntrypoint = true
		}
		if usingHostEntrypoint {
			fmt.Fprintf(os.Stderr, "warning: command %q is not in the image; using host executable %s\n", command[0], hostCommand)
		}
		cmd = exec.Command(hostCommand, command[1:]...)
		cmd.Dir = filepath.Join(extractDir, filepath.FromSlash(strings.TrimPrefix(workdir, "/")))
		// Keep non-TTY workloads in their own process group so signals can
		// reach the complete workload process tree. A TTY workload must remain
		// in the caller's terminal process group or it cannot read the
		// caller's terminal without receiving SIGTTIN.
		if !*tty {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
	}
	cmd.Env = envVars
	if *interactive || *tty {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		if *chroot {
			return fmt.Errorf("start %s (chroot=%s): %w; try without --chroot to test the Mach-O executable first", imageCommand, extractDir, err)
		}
		return fmt.Errorf("start %s: %w", imageCommand, err)
	}
	startedAt := time.Now().UTC()
	if *pidFile != "" {
		processGroup := 1
		if *tty {
			processGroup = 0
		}
		pidData := []byte(fmt.Sprintf("%d %d %d\n", cmd.Process.Pid, os.Getpid(), processGroup))
		if err := os.WriteFile(*pidFile, pidData, 0o600); err != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = cmd.Wait()
			return fmt.Errorf("write workload PID file: %w", err)
		}
		defer os.Remove(*pidFile)
	}

	// Setpgid lets Ctrl-C and termination reach descendants of the workload.
	// This is process-group management, not PID namespace isolation.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		sig := <-signals
		if signal, ok := sig.(syscall.Signal); ok && cmd.Process != nil {
			target := cmd.Process.Pid
			if !*tty {
				target = -target
			}
			_ = syscall.Kill(target, signal)
		}
	}()

	waitErr := cmd.Wait()
	finishedAt := time.Now().UTC()
	exitInfo := processExitInfoFromState(cmd.Process.Pid, cmd.ProcessState, startedAt, finishedAt)
	if *statusFile != "" {
		if err := writeProcessExitInfo(*statusFile, exitInfo); err != nil {
			if waitErr != nil {
				return errors.Join(workloadExitErrorFor(waitErr, exitInfo), err)
			}
			return err
		}
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return workloadExitErrorFor(waitErr, exitInfo)
		}
		return waitErr
	}
	return nil
}

func workloadExitErrorFor(waitErr error, info processExitInfo) error {
	return &workloadExitError{
		err:  fmt.Errorf("workload exited: %w", waitErr),
		info: info,
	}
}

func processExitInfoFromState(pid int, state *os.ProcessState, startedAt, finishedAt time.Time) processExitInfo {
	info := processExitInfo{
		PID:               pid,
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		TerminationReason: "exited",
	}
	if state == nil {
		info.TerminationReason = "unknown"
		return info
	}
	waitStatus, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		code := state.ExitCode()
		info.ExitCode = &code
		return info
	}
	if waitStatus.Signaled() {
		code := 128 + int(waitStatus.Signal())
		info.ExitCode = &code
		info.TerminationSignal = signalName(waitStatus.Signal())
		info.TerminationReason = "signal"
		return info
	}
	code := waitStatus.ExitStatus()
	info.ExitCode = &code
	if code != 0 {
		info.TerminationReason = "exited-with-error"
	}
	return info
}

func signalName(signal syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP:  "SIGHUP",
		syscall.SIGINT:  "SIGINT",
		syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGILL:  "SIGILL",
		syscall.SIGABRT: "SIGABRT",
		syscall.SIGFPE:  "SIGFPE",
		syscall.SIGKILL: "SIGKILL",
		syscall.SIGSEGV: "SIGSEGV",
		syscall.SIGPIPE: "SIGPIPE",
		syscall.SIGALRM: "SIGALRM",
		syscall.SIGTERM: "SIGTERM",
		syscall.SIGUSR1: "SIGUSR1",
		syscall.SIGUSR2: "SIGUSR2",
		syscall.SIGCHLD: "SIGCHLD",
		syscall.SIGCONT: "SIGCONT",
		syscall.SIGSTOP: "SIGSTOP",
		syscall.SIGTSTP: "SIGTSTP",
		syscall.SIGTTIN: "SIGTTIN",
		syscall.SIGTTOU: "SIGTTOU",
	}
	if name, ok := names[signal]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", signal)
}

func writeProcessExitInfo(path string, info processExitInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workload exit information: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create workload status directory: %w", err)
	}
	temporary := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write workload exit information: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("install workload exit information: %w", err)
	}
	return nil
}

func readProcessExitInfo(path string) (processExitInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return processExitInfo{}, err
	}
	var info processExitInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return processExitInfo{}, fmt.Errorf("decode workload exit information: %w", err)
	}
	if info.PID <= 0 || info.FinishedAt.IsZero() {
		return processExitInfo{}, errors.New("workload exit information is incomplete")
	}
	return info, nil
}

func buildImage(opts buildOptions) (err error) {
	info, err := os.Stat(opts.RootFS)
	if err != nil {
		return fmt.Errorf("stat rootfs: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rootfs %q is not a directory", opts.RootFS)
	}
	if opts.Tag == "" {
		return errors.New("image tag must not be empty")
	}
	if opts.Architecture == "" {
		return errors.New("image architecture must not be empty")
	}

	if _, statErr := os.Lstat(opts.Output); statErr == nil {
		return fmt.Errorf("output %q already exists", opts.Output)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("check output: %w", statErr)
	}
	if err := os.MkdirAll(filepath.Join(opts.Output, "blobs", "sha256"), 0o755); err != nil {
		return fmt.Errorf("create OCI layout: %w", err)
	}
	createdOutput := true
	defer func() {
		if err != nil && createdOutput {
			_ = os.RemoveAll(opts.Output)
		}
	}()

	layerPath, layerDigest, layerSize, err := makeLayer(opts.RootFS)
	if err != nil {
		return err
	}
	defer os.Remove(layerPath)
	if err := installBlobFile(opts.Output, layerDigest, layerPath); err != nil {
		return fmt.Errorf("write layer blob: %w", err)
	}

	created := time.Now().UTC().Format(time.RFC3339Nano)
	config := imageConfig{
		Created:      created,
		Architecture: opts.Architecture,
		OS:           "darwin",
		Config: runtimeConfig{
			Env:        append([]string(nil), opts.Env...),
			Entrypoint: []string{opts.Entrypoint},
			Cmd:        append([]string(nil), opts.Args...),
			WorkingDir: opts.WorkingDir,
		},
		RootFS: rootFS{
			Type:    "layers",
			DiffIDs: []string{layerDigest},
		},
		History: []historyEntry{{Created: created, CreatedBy: "macker oci build"}},
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode image config: %w", err)
	}
	configDigest := digestBytes(configBytes)
	if err := installBlobBytes(opts.Output, configDigest, configBytes); err != nil {
		return fmt.Errorf("write config blob: %w", err)
	}

	man := manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config: descriptor{
			MediaType: configMediaType,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
		Layers: []descriptor{{
			MediaType: layerMediaType,
			Digest:    layerDigest,
			Size:      layerSize,
		}},
	}
	manifestBytes, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestDigest := digestBytes(manifestBytes)
	if err := installBlobBytes(opts.Output, manifestDigest, manifestBytes); err != nil {
		return fmt.Errorf("write manifest blob: %w", err)
	}

	idx := imageIndex{
		SchemaVersion: 2,
		Manifests: []descriptor{{
			MediaType: manifestMediaType,
			Digest:    manifestDigest,
			Size:      int64(len(manifestBytes)),
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": opts.Tag,
			},
			Platform: &platform{
				OS:           "darwin",
				Architecture: opts.Architecture,
			},
		}},
	}
	if err := writeJSON(filepath.Join(opts.Output, "oci-layout"), imageLayout{ImageLayoutVersion: layoutVersion}); err != nil {
		return fmt.Errorf("write oci-layout: %w", err)
	}
	if err := writeJSON(filepath.Join(opts.Output, "index.json"), idx); err != nil {
		return fmt.Errorf("write index: %w", err)
	}

	fmt.Printf("created %s (%s/%s)\n", opts.Output, config.OS, config.Architecture)
	fmt.Printf("manifest: %s\n", manifestDigest)
	return nil
}

func makeLayer(root string) (path string, digest string, size int64, err error) {
	file, err := os.CreateTemp("", "macker-layer-*.tar")
	if err != nil {
		return "", "", 0, fmt.Errorf("create temporary layer: %w", err)
	}
	path = file.Name()
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()

	tw := tar.NewWriter(file)
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}

		var linkName string
		if info.Mode()&os.ModeSymlink != 0 {
			linkName, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type in rootfs: %s (%s)", rel, info.Mode())
		}

		header, err := tar.FileInfoHeader(info, linkName)
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		}
		// Image layers should not change merely because the host file mtime
		// changed. File modes are retained; ownership and timestamps are not.
		header.ModTime = time.Unix(0, 0).UTC()
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.Format = tar.FormatPAX
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			input, err := os.Open(filePath)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, input)
			closeErr := input.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	if err != nil {
		_ = tw.Close()
		return "", "", 0, fmt.Errorf("create layer: %w", err)
	}
	if err := tw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("finish layer: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("sync layer: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", "", 0, fmt.Errorf("close layer: %w", err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		return "", "", 0, err
	}
	digest, err = digestFile(path)
	if err != nil {
		return "", "", 0, err
	}
	return path, digest, stat.Size(), nil
}

func loadImage(layout, tag string) (loadedImage, error) {
	layoutInfo, err := os.Stat(layout)
	if err != nil {
		return loadedImage{}, fmt.Errorf("stat image layout: %w", err)
	}
	if !layoutInfo.IsDir() {
		return loadedImage{}, fmt.Errorf("image layout %q is not a directory", layout)
	}
	var layoutJSON imageLayout
	if err := readJSONFile(filepath.Join(layout, "oci-layout"), &layoutJSON); err != nil {
		return loadedImage{}, fmt.Errorf("read oci-layout: %w", err)
	}
	if layoutJSON.ImageLayoutVersion != layoutVersion {
		return loadedImage{}, fmt.Errorf("unsupported OCI layout version %q", layoutJSON.ImageLayoutVersion)
	}
	var idx imageIndex
	if err := readJSONFile(filepath.Join(layout, "index.json"), &idx); err != nil {
		return loadedImage{}, fmt.Errorf("read index: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return loadedImage{}, errors.New("OCI index contains no manifests")
	}

	selected := -1
	tagSelected := -1
	nativeSelected := -1
	for i, candidate := range idx.Manifests {
		ref := candidate.Annotations["org.opencontainers.image.ref.name"]
		if tag != "" && ref == tag {
			tagSelected = i
		}
		if candidate.Platform != nil && candidate.Platform.OS == "darwin" && candidate.Platform.Architecture == runtime.GOARCH {
			nativeSelected = i
		}
	}
	// A bundled OCI layout has one root descriptor per platform, while its
	// ref.name annotation is attached to only one descriptor. Prefer the
	// native descriptor so macker run can use a bundled image even when the
	// tagged descriptor happens to be Linux.
	if nativeSelected != -1 {
		selected = nativeSelected
	} else if tagSelected != -1 {
		selected = tagSelected
	} else if tag != "" {
		return loadedImage{}, fmt.Errorf("reference %q not found", tag)
	} else {
		selected = 0
	}
	selectedDescriptor := idx.Manifests[selected]
	var man manifest
	if err := readDescriptorJSON(layout, selectedDescriptor, &man); err != nil {
		return loadedImage{}, fmt.Errorf("read manifest: %w", err)
	}
	if man.SchemaVersion != 2 {
		return loadedImage{}, fmt.Errorf("unsupported manifest schema version %d", man.SchemaVersion)
	}
	var config imageConfig
	if err := readDescriptorJSON(layout, man.Config, &config); err != nil {
		return loadedImage{}, fmt.Errorf("read image config: %w", err)
	}
	return loadedImage{
		Layout:    layout,
		Reference: selectedDescriptor.Annotations["org.opencontainers.image.ref.name"],
		Manifest:  man,
		Config:    config,
	}, nil
}

func checkPlatform(config imageConfig) error {
	if config.OS != "darwin" {
		return fmt.Errorf("image OS is %q, not darwin", config.OS)
	}
	if config.Architecture != runtime.GOARCH {
		return fmt.Errorf("image architecture is %q, host architecture is %q", config.Architecture, runtime.GOARCH)
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("running Darwin workloads requires macOS; current host is %q", runtime.GOOS)
	}
	return nil
}

func unpackImage(img loadedImage, output string, force bool) error {
	if err := prepareOutputDir(output, force); err != nil {
		return err
	}
	for i, layer := range img.Manifest.Layers {
		reader, err := openLayer(img.Layout, layer)
		if err != nil {
			return fmt.Errorf("open layer %d: %w", i, err)
		}
		err = applyTarLayer(output, reader)
		closeErr := reader.Close()
		if err != nil {
			return fmt.Errorf("apply layer %d: %w", i, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close layer %d: %w", i, closeErr)
		}
	}
	return nil
}

func prepareOutputDir(dir string, force bool) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output %q is not a normal directory", dir)
		}
		if force {
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove output: %w", err)
			}
			return os.MkdirAll(dir, 0o755)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("output %q is not empty (use --force to replace it)", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

func applyTarLayer(root string, input io.Reader) error {
	tr := tar.NewReader(input)
	directoryModes := make(map[string]fs.FileMode)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel, err := cleanArchivePath(header.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		if strings.HasPrefix(path.Base(rel), ".wh.") {
			if err := applyWhiteout(root, rel); err != nil {
				return err
			}
			continue
		}

		target, err := safePath(root, rel)
		if err != nil {
			return err
		}
		parentRel := path.Dir(rel)
		if parentRel == "." {
			parentRel = ""
		}
		if err := ensureOCINoSymlinkParents(root, parentRel); err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensureDirectory(target); err != nil {
				return fmt.Errorf("create directory %q: %w", rel, err)
			}
			directoryModes[rel] = fs.FileMode(header.Mode).Perm()
		case tar.TypeReg, tar.TypeRegA:
			if err := removeOCIExisting(target); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create file %q: %w", rel, err)
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract file %q: %w", rel, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
			if err := os.Chmod(target, fs.FileMode(header.Mode).Perm()); err != nil {
				return fmt.Errorf("chmod %q: %w", rel, err)
			}
		case tar.TypeSymlink:
			if err := removeOCIExisting(target); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("create symlink %q: %w", rel, err)
			}
		case tar.TypeLink:
			linkRel, err := cleanArchivePath(header.Linkname)
			if err != nil {
				return fmt.Errorf("hardlink %q: %w", rel, err)
			}
			linkTarget, err := safePath(root, linkRel)
			if err != nil {
				return err
			}
			if err := ensureOCINoSymlinkParents(root, path.Dir(linkRel)); err != nil {
				return err
			}
			if err := removeOCIExisting(target); err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("create hardlink %q: %w", rel, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry %q (type %d)", rel, header.Typeflag)
		}
	}

	// Apply directory modes last so a restrictive lower-layer directory does
	// not prevent a later layer from adding files to it.
	var dirs []string
	for rel := range directoryModes {
		dirs = append(dirs, rel)
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, rel := range dirs {
		target, err := safePath(root, rel)
		if err != nil {
			return err
		}
		if err := os.Chmod(target, directoryModes[rel]); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("chmod directory %q: %w", rel, err)
		}
	}
	return nil
}

func applyWhiteout(root, rel string) error {
	base := path.Base(rel)
	parent := path.Dir(rel)
	if parent == "." {
		parent = ""
	}
	if err := ensureOCINoSymlinkParents(root, parent); err != nil {
		return err
	}
	parentPath, err := safePath(root, parent)
	if err != nil {
		return err
	}
	if base == ".wh..wh..opq" {
		entries, err := os.ReadDir(parentPath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(parentPath, entry.Name())); err != nil {
				return fmt.Errorf("remove opaque entry %q: %w", entry.Name(), err)
			}
		}
		return nil
	}
	name := strings.TrimPrefix(base, ".wh.")
	if name == "" {
		return fmt.Errorf("invalid whiteout %q", rel)
	}
	target, err := safePath(root, path.Join(parent, name))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove whiteout target %q: %w", rel, err)
	}
	return nil
}

func cleanArchivePath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return clean, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func safePath(root, rel string) (string, error) {
	if rel == "" || rel == "." {
		return root, nil
	}
	clean, err := cleanArchivePath(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func ensureOCINoSymlinkParents(root, rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	clean, err := cleanArchivePath(rel)
	if err != nil {
		return err
	}
	current := root
	for _, component := range strings.Split(clean, "/") {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive path traverses symlink %q", rel)
		}
		if !info.IsDir() {
			return fmt.Errorf("archive path parent %q is not a directory", rel)
		}
	}
	return nil
}

func ensureDirectory(target string) error {
	info, err := os.Lstat(target)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err := removeOCIExisting(target); err != nil {
				return err
			}
			return os.Mkdir(target, 0o755)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(target, 0o755)
}

func removeOCIExisting(target string) error {
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(target)
	}
	return os.Remove(target)
}

func openLayer(layout string, layer descriptor) (io.ReadCloser, error) {
	file, err := openBlob(layout, layer.Digest)
	if err != nil {
		return nil, err
	}
	if layer.MediaType != gzipLayerMediaType && !strings.HasSuffix(layer.MediaType, "+gzip") {
		return file, nil
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &compoundReadCloser{Reader: gz, closers: []io.Closer{gz, file}}, nil
}

type compoundReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compoundReadCloser) Close() error {
	var first error
	for _, closer := range c.closers {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func selectImageCommand(entrypointOverride string, imageEntrypoint, imageCmd, commandArgs, commandOverride []string) []string {
	command := append([]string(nil), imageEntrypoint...)
	if entrypointOverride != "" {
		command = []string{entrypointOverride}
	}
	command = append(command, imageCmd...)
	command = append(command, commandArgs...)
	if len(commandOverride) > 0 {
		if entrypointOverride != "" {
			command = append([]string{entrypointOverride}, commandOverride...)
		} else {
			command = append([]string(nil), commandOverride...)
		}
	}
	if len(command) == 0 {
		command = []string{defaultImageCommand}
	}
	return command
}

func isExecutableFile(filename string) bool {
	info, err := os.Stat(filename)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func resolveHostCommand(command string) (string, error) {
	if strings.Contains(command, "/") {
		if !strings.HasPrefix(command, "/") {
			return "", fmt.Errorf("host entrypoint path %q is not absolute", command)
		}
		if !isExecutableFile(command) {
			return "", fmt.Errorf("host entrypoint %q is not executable", command)
		}
		return command, nil
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveImageCommand(rootfs, command, workdir string, env []string) (string, error) {
	if command == "" {
		return "", errors.New("image command is empty")
	}
	if strings.Contains(command, "/") {
		if strings.HasPrefix(command, "/") {
			return path.Clean(command), nil
		}
		return path.Join(workdir, command), nil
	}

	pathValue := environmentValue(env, "PATH")
	if pathValue == "" {
		pathValue = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for _, dir := range strings.Split(pathValue, ":") {
		if dir == "" {
			dir = workdir
		}
		candidate := path.Join(dir, command)
		candidateHost, err := safePath(rootfs, candidate)
		if err != nil {
			continue
		}
		info, statErr := os.Stat(candidateHost)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not resolve %q in image PATH", command)
}

func mergeEnvironment(base, overrides []string) []string {
	result := append([]string(nil), base...)
	indexes := make(map[string]int, len(result))
	for i, value := range result {
		if key, _, ok := strings.Cut(value, "="); ok {
			indexes[key] = i
		}
	}
	for _, value := range overrides {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			continue
		}
		if i, exists := indexes[key]; exists {
			result[i] = value
		} else {
			indexes[key] = len(result)
			result = append(result, value)
		}
	}
	return result
}

func environmentValue(env []string, key string) string {
	for _, value := range env {
		if name, result, ok := strings.Cut(value, "="); ok && name == key {
			return result
		}
	}
	return ""
}

func ensureTerminalEnvironment(env []string) []string {
	for _, value := range env {
		if name, _, ok := strings.Cut(value, "="); ok && name == "TERM" {
			return env
		}
	}
	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}
	return append(append([]string(nil), env...), "TERM="+term)
}

func digestBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func digestFile(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func blobPath(layout, digest string) (string, error) {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("invalid digest %q: %w", digest, err)
	}
	return filepath.Join(layout, "blobs", algorithm, encoded), nil
}

func openBlob(layout, digest string) (*os.File, error) {
	name, err := blobPath(layout, digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", digest, err)
	}
	return file, nil
}

func readDescriptorJSON(layout string, descriptor descriptor, target any) error {
	file, err := openBlob(layout, descriptor.Digest)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(data)) != descriptor.Size {
		return fmt.Errorf("blob %s has size %d, expected %d", descriptor.Digest, len(data), descriptor.Size)
	}
	if actual := digestBytes(data); actual != descriptor.Digest {
		return fmt.Errorf("blob digest is %s, expected %s", actual, descriptor.Digest)
	}
	return json.Unmarshal(data, target)
}

func installBlobBytes(layout, digest string, data []byte) error {
	blob, err := blobPath(layout, digest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(blob); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWrite(blob, data)
}

func installBlobFile(layout, digest, source string) error {
	blob, err := blobPath(layout, digest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(blob); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(blob), ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	input, err := os.Open(source)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	_, copyErr := io.Copy(tmp, input)
	closeInputErr := input.Close()
	closeTmpErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeInputErr != nil {
		return closeInputErr
	}
	if closeTmpErr != nil {
		return closeTmpErr
	}
	return os.Rename(tmpName, blob)
}

func atomicWrite(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, name)
}

func writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(name, data)
}

func readJSONFile(name string, value any) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

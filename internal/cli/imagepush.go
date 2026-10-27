package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/spf13/cobra"
)

const maxImagePushBytes int64 = 8 << 30

var (
	imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	imageDigestRef  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type rpcImagePushParams struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type rpcImagePushResult struct {
	Reference    string `json:"reference"`
	TagReference string `json:"tag_reference"`
	Digest       string `json:"digest"`
	SizeBytes    int64  `json:"size_bytes"`
}

func (a *app) imageSpecs() []spec {
	parent := operationMeta("Move a locally built Docker image into the node's local registry", "eregion image push andurel-site:local --app andurel --json")
	parent.WhenToUse = []string{"Push an image that exists only on the operator's machine into the main node's loopback registry, so an environment can deploy it by immutable digest."}
	parent.Examples = []string{"eregion image push andurel-site:local --app andurel --json"}

	push := operationMeta("Stream a local Docker image through node RPC into the node's local registry", "eregion image push andurel-site:local --app andurel --tag local-2026-10-27 --json")
	push.WhenToUse = []string{"Push a locally built image without an external registry. The image streams over SSH inside WireGuard; there is no registry port forwarding or temporary file. The default tag is the local image's short ID."}
	push.Mutating = true
	push.DryRunReason = "The image is streamed to the node and loaded into its registry; there is no safe local preview of the transfer."
	push.Examples = []string{
		"eregion image push andurel-site:local --app andurel",
		"eregion image push andurel-site:local --app andurel --tag release-42 --json",
	}
	push.Next = []string{"eregion env update <app> <environment> --image <reference>"}

	return []spec{{
		use:  "image",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "push <local-image>",
				meta: push,
				args: imagePushArg,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("app", "", "Application name; also the registry repository")
					cmd.Flags().String("tag", "", "Image tag; defaults to the local image's short ID")
				},
				run: a.imagePush,
			},
		},
	}}
}

func imagePushArg(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return contract.NewFault(contract.InputRequired, "local image is required", "Use the Docker image reference on this machine, for example andurel-site:local.")
	}
	return platform.ValidateImage(strings.TrimSpace(args[0]))
}

func (a *app) imagePush(cmd *cobra.Command, args []string) (result, error) {
	localImage := strings.TrimSpace(args[0])
	if err := platform.ValidateImage(localImage); err != nil {
		return result{}, err
	}
	appName := strings.TrimSpace(stringFlag(cmd, "app"))
	if appName == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--app is required", "Name the application the image belongs to, for example --app andurel.")
	}
	if err := platform.ValidateName(appName); err != nil {
		return result{}, err
	}
	ctx := cmd.Context()
	if _, err := exec.LookPath("docker"); err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "local Docker is not available", "Install Docker and make `docker` available on PATH before pushing an image.")
	}
	imageID, err := localDockerImageID(ctx, localImage)
	if err != nil {
		return result{}, err
	}
	tag := strings.TrimSpace(stringFlag(cmd, "tag"))
	if tag == "" {
		tag = defaultImageTag(imageID)
	}
	if !imageTagPattern.MatchString(tag) {
		return result{}, contract.NewFault(contract.ValidationFailed, "invalid image tag", "Use 1–128 letters, digits, dots, underscores, or hyphens.")
	}
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}

	saveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	save := exec.CommandContext(saveCtx, "docker", "save", localImage) //nolint:gosec // G204: fixed argv, operator-supplied image reference.
	stdout, err := save.StdoutPipe()
	if err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "could not start local docker save", err.Error())
	}
	var saveErr bytes.Buffer
	save.Stderr = &saveErr
	if err := save.Start(); err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "could not start local docker save", boundedDockerOutput(err.Error()))
	}
	pushed, pushErr := client.PushImage(saveCtx, appName, tag, stdout, localImage)
	if pushErr != nil {
		// Stop a save that is still running when the node refused the stream.
		cancel()
		_ = stdout.Close()
		_ = save.Wait()
		return result{}, pushErr
	}
	// The node read the whole stream; let docker save finish on its own.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := save.Wait()
	if waitErr != nil {
		if ctx.Err() != nil {
			return result{}, ctx.Err()
		}
		detail := boundedDockerOutput(saveErr.String())
		if detail == "" {
			detail = boundedDockerOutput(waitErr.Error())
		}
		return result{}, contract.NewFault(contract.Unavailable, "local docker save failed", detail)
	}

	node := client.contextName
	if node == "" {
		node = client.remoteTarget()
	}
	plain := fmt.Sprintf("Pushed %s to %s\nreference: %s\ntag: %s\nsize: %s\nnext: eregion env update %s <environment> --image %s",
		localImage, node, pushed.Reference, pushed.TagReference, imageSize(pushed.SizeBytes), appName, pushed.Reference)
	next := []string{fmt.Sprintf("eregion env update %s <environment> --image %s", appName, pushed.Reference)}
	return result{data: pushed, plain: plain, next: next}, nil
}

func localDockerImageID(ctx context.Context, image string) (string, error) {
	command := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image) //nolint:gosec // G204: fixed argv, validated image reference.
	var stderr bytes.Buffer
	command.Stderr = &stderr
	raw, err := command.Output()
	if err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", contract.NewFault(contract.PreconditionFailed, "local Docker is not available", "Install Docker and make `docker` available on PATH before pushing an image.")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	detail := boundedDockerOutput(stderr.String())
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "no such image"):
		return "", contract.NewFault(contract.NotFound, "local image "+image+" was not found", "Build or pull the image on this machine first; `docker image ls` lists local images.")
	case strings.Contains(lower, "cannot connect to the docker daemon"):
		return "", contract.NewFault(contract.PreconditionFailed, "local Docker is not running", "Start Docker and try again.")
	case detail != "":
		return "", contract.NewFault(contract.Unavailable, "local Docker could not inspect "+image, detail)
	default:
		return "", contract.NewFault(contract.Unavailable, "local Docker could not inspect "+image, err.Error())
	}
}

func defaultImageTag(imageID string) string {
	id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(imageID)), "sha256:")
	if len(id) >= 12 && imageTagPattern.MatchString(id[:12]) {
		return id[:12]
	}
	return time.Now().UTC().Format("20060102-150405")
}

// PushImage streams a docker save tar after the request line and returns the
// digest the node read back from its local registry.
func (n *nodeClient) PushImage(ctx context.Context, repository, tag string, payload io.Reader, localImage string) (rpcImagePushResult, error) {
	node := n.contextName
	if node == "" {
		node = n.remoteTarget()
	}
	raw, err := n.call(ctx, methodImagePush, rpcImagePushParams{Repository: repository, Tag: tag}, rpcCallOptions{
		Payload: payload, Stream: true, Progress: true,
		Label: "Pushing " + localImage + " to " + node + "…",
	})
	if err != nil {
		return rpcImagePushResult{}, err
	}
	var pushed rpcImagePushResult
	if json.Unmarshal(raw, &pushed) != nil || pushed.Reference == "" || pushed.TagReference == "" || pushed.Digest == "" || pushed.SizeBytes < 0 {
		return rpcImagePushResult{}, invalidNodeData("image push result")
	}
	return pushed, nil
}

func validateImagePushParams(params rpcImagePushParams) error {
	if err := platform.ValidateName(params.Repository); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid image repository", "Use the app name: 1–40 lowercase letters, digits, and inner hyphens; start with a letter and end with a letter or digit.")
	}
	if !imageTagPattern.MatchString(params.Tag) {
		return contract.NewFault(contract.ValidationFailed, "invalid image tag", "Use 1–128 letters, digits, dots, underscores, or hyphens.")
	}
	return nil
}

// nodeRegistryAddress follows the recorded host intent so a non-default
// loopback registry address is used for tagging and the digest lookup.
func nodeRegistryAddress() string {
	if intent, err := host.LoadIntent(host.DefaultLayout()); err == nil && strings.TrimSpace(intent.RegistryListen) != "" {
		return strings.TrimSpace(intent.RegistryListen)
	}
	return host.DefaultConfig(host.RoleMain, "").RegistryListen
}

func pushNodeImage(ctx context.Context, params rpcImagePushParams, payload io.Reader, out io.Writer) (rpcImagePushResult, error) {
	if err := validateImagePushParams(params); err != nil {
		return rpcImagePushResult{}, err
	}
	if payload == nil {
		payload = strings.NewReader("")
	}
	progress := &imagePushProgress{out: out}
	size, loaded, err := loadDockerImage(ctx, payload, progress)
	if err != nil {
		return rpcImagePushResult{}, err
	}
	registry := nodeRegistryAddress()
	repository := registry + "/" + params.Repository
	tagReference := repository + ":" + params.Tag
	if _, err := runDockerImageCommand(ctx, []string{"docker", "tag", loaded, tagReference}, progress, false); err != nil {
		return rpcImagePushResult{}, err
	}
	progress.emit("pushing " + tagReference)
	if _, err := runDockerImageCommand(ctx, []string{"docker", "push", tagReference}, progress, true); err != nil {
		return rpcImagePushResult{}, err
	}
	raw, err := runDockerImageCommand(ctx, []string{"docker", "inspect", "--format", "{{json .RepoDigests}}", tagReference}, nil, false)
	if err != nil {
		return rpcImagePushResult{}, err
	}
	reference, digest, err := selectRepoDigest(raw, repository)
	if err != nil {
		return rpcImagePushResult{}, err
	}
	progress.emit("pushed " + digest)
	return rpcImagePushResult{Reference: reference, TagReference: tagReference, Digest: digest, SizeBytes: size}, nil
}

func loadDockerImage(ctx context.Context, payload io.Reader, progress *imagePushProgress) (int64, string, error) {
	reader := &boundedImagePayload{reader: payload, max: maxImagePushBytes, progress: progress}
	command := exec.CommandContext(ctx, "docker", "load")
	command.Stdin = reader
	output := &lockedBuffer{}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if reader.exceeded {
		return reader.read, "", contract.NewFault(contract.PreconditionFailed, "the image stream is larger than 8 GiB", "Push a smaller image; the node refuses an unbounded stream.")
	}
	if err != nil {
		if ctx.Err() != nil {
			return reader.read, "", ctx.Err()
		}
		if reader.read == 0 {
			return 0, "", contract.NewFault(contract.InputRequired, "the image payload is empty", "Send a docker save tar after the request line.")
		}
		return reader.read, "", contract.NewFault(contract.Unavailable, "the node could not load the image", boundedDockerOutput(output.String()))
	}
	loaded, err := parseDockerLoadOutput(output.String())
	if err != nil {
		return reader.read, "", contract.NewFault(contract.Unavailable, "docker load did not report a loaded image", err.Error())
	}
	if err := platform.ValidateImage(loaded); err != nil {
		return reader.read, "", contract.NewFault(contract.Unavailable, "docker load reported an invalid image reference", loaded)
	}
	progress.emit("loaded " + loaded)
	return reader.read, loaded, nil
}

func parseDockerLoadOutput(output string) (string, error) {
	var loaded, imageID string
	for _, raw := range strings.FieldsFunc(output, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line := strings.TrimSpace(raw)
		if value, ok := strings.CutPrefix(line, "Loaded image ID:"); ok {
			if value = strings.TrimSpace(value); value != "" {
				imageID = value
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "Loaded image:"); ok {
			if value = strings.TrimSpace(value); value != "" {
				loaded = value
			}
		}
	}
	if loaded != "" {
		return loaded, nil
	}
	if imageID != "" {
		return imageID, nil
	}
	return "", errors.New("no \"Loaded image:\" line in docker load output")
}

func selectRepoDigest(raw []byte, repository string) (string, string, error) {
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &digests); err != nil {
		return "", "", contract.NewFault(contract.Unavailable, "the node returned an invalid image digest list", err.Error())
	}
	prefix := repository + "@"
	for _, reference := range digests {
		if !strings.HasPrefix(reference, prefix) {
			continue
		}
		digest := strings.TrimPrefix(reference, prefix)
		if imageDigestRef.MatchString(digest) {
			return reference, digest, nil
		}
	}
	return "", "", contract.NewFault(contract.PreconditionFailed, "the pushed image has no registry digest", "Check that the node's local registry is running, then push the image again.")
}

func runDockerImageCommand(ctx context.Context, argv []string, progress *imagePushProgress, stream bool) ([]byte, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: fixed argv built from validated values.
	var output []byte
	var err error
	if stream && progress != nil {
		writer := &dockerProgressWriter{progress: progress}
		command.Stdout, command.Stderr = writer, writer
		err = command.Run()
		output = writer.Bytes()
	} else {
		output, err = command.CombinedOutput()
	}
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	detail := boundedDockerOutput(string(output))
	if detail == "" {
		detail = err.Error()
	}
	return nil, contract.NewFault(contract.Unavailable, "the node could not run docker "+strings.Join(argv[1:2], " "), detail)
}

type boundedImagePayload struct {
	reader   io.Reader
	max      int64
	read     int64
	exceeded bool
	progress *imagePushProgress
	lastEmit time.Time
}

func (r *boundedImagePayload) Read(p []byte) (int, error) {
	if r.read >= r.max {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			r.exceeded = true
			return 0, errors.New("image stream exceeds the 8 GiB limit")
		}
		return 0, err
	}
	if remaining := r.max - r.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	r.emitProgress()
	return n, err
}

func (r *boundedImagePayload) emitProgress() {
	if r.progress == nil {
		return
	}
	now := time.Now()
	if !r.lastEmit.IsZero() && now.Sub(r.lastEmit) < time.Second && r.read < r.max {
		return
	}
	r.lastEmit = now
	r.progress.emit("received " + imageSize(r.read))
}

type imagePushProgress struct {
	mu  sync.Mutex
	out io.Writer
	err error
}

func (p *imagePushProgress) emit(message string) {
	if p == nil || p.out == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if len(message) > 512 {
		message = message[:512] + "…"
	}
	raw, err := rpc.MarshalEvent(rpc.EventBody{Kind: "log", Message: message})
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	if _, err := p.out.Write(raw); err != nil {
		p.err = err
	}
}

// lockedBuffer collects stdout and stderr from one child without racing the
// two copier goroutines os/exec starts for non-file writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type dockerProgressWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	pending  []byte
	progress *imagePushProgress
	last     time.Time
}

func (w *dockerProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexAny(w.pending, "\r\n")
		if index < 0 {
			break
		}
		line := string(w.pending[:index])
		w.pending = w.pending[index+1:]
		w.emitLine(line)
	}
	return len(p), nil
}

func (w *dockerProgressWriter) emitLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" || w.progress == nil {
		return
	}
	if !w.last.IsZero() && time.Since(w.last) < time.Second {
		return
	}
	w.last = time.Now()
	w.progress.emit(line)
}

func (w *dockerProgressWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]byte(nil), w.buf.Bytes()...)
	if len(w.pending) > 0 {
		out = append(out, w.pending...)
	}
	return out
}

func imageSize(bytes int64) string {
	if bytes < 1<<20 {
		return fmt.Sprintf("%d KB", (bytes+1023)/1024)
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/(1<<20))
}

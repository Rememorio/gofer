package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	imagePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]{0,254}$`)
	memoryPattern = regexp.MustCompile(`^[1-9][0-9]*(?:[bkmgBKMG])?$`)
)

// DockerConfig configures an ephemeral, hardened Docker command executor.
type DockerConfig struct {
	Binary         string
	Image          string
	Mounts         []Mount
	NetworkEnabled bool
	User           string
	Memory         string
	CPUs           float64
	PIDsLimit      int
	TmpfsSizeBytes int64
	MaxOutputBytes int64
	MaxScriptBytes int
	CommandTimeout time.Duration
	MaxTimeout     time.Duration
}

// Docker executes each command in a fresh read-only container with bounded
// resources and an optional network namespace.
type Docker struct {
	binary         string
	image          string
	mounts         []Mount
	networkEnabled bool
	user           string
	memory         string
	cpus           float64
	pidsLimit      int
	tmpfsSizeBytes int64
	limits         limits
}

// NewDocker validates config and constructs an ephemeral container executor.
func NewDocker(config DockerConfig) (*Docker, error) {
	if config.Binary == "" {
		config.Binary = "docker"
	}
	if strings.TrimSpace(config.Binary) != config.Binary || strings.IndexByte(config.Binary, 0) >= 0 {
		return nil, fmt.Errorf("%w: Docker binary is invalid", ErrInvalidConfig)
	}
	if !imagePattern.MatchString(config.Image) {
		return nil, fmt.Errorf("%w: Docker image is invalid", ErrInvalidConfig)
	}
	mounts, err := validateMounts(config.Mounts)
	if err != nil {
		return nil, err
	}
	if working, found := findMount(mounts, "/mnt/user-data/workspace"); !found || working.ReadOnly {
		return nil, fmt.Errorf("%w: a writable /mnt/user-data/workspace mount is required", ErrInvalidConfig)
	}
	limits, err := newLimits(config.MaxOutputBytes, config.MaxScriptBytes, config.CommandTimeout, config.MaxTimeout)
	if err != nil {
		return nil, err
	}
	applyDockerDefaults(&config)
	if err := validateDockerResources(config); err != nil {
		return nil, err
	}
	return &Docker{
		binary: config.Binary, image: config.Image, mounts: mounts,
		networkEnabled: config.NetworkEnabled, user: config.User,
		memory: config.Memory, cpus: config.CPUs, pidsLimit: config.PIDsLimit,
		tmpfsSizeBytes: config.TmpfsSizeBytes, limits: limits,
	}, nil
}

// Available reports whether the configured Docker client binary is present.
func (docker *Docker) Available() bool {
	if docker == nil {
		return false
	}
	_, err := exec.LookPath(docker.binary)
	return err == nil
}

// Execute runs one command in a fresh hardened container.
func (docker *Docker) Execute(ctx context.Context, command Command) (Result, error) {
	if docker == nil {
		return Result{}, fmt.Errorf("%w: Docker executor is nil", ErrInvalidConfig)
	}
	if !docker.Available() {
		return Result{}, fmt.Errorf("%w: Docker client %q was not found", ErrUnavailable, docker.binary)
	}
	command, err := docker.limits.validate(command)
	if err != nil {
		return Result{}, err
	}
	invocation, err := docker.buildInvocation(command)
	if err != nil {
		return Result{}, err
	}
	defer invocation.cleanup()
	result, executeErr := executeProcess(ctx, processSpec{
		name: docker.binary, arguments: invocation.arguments,
		environment: os.Environ(), timeout: command.Timeout,
		maxOutput: docker.limits.maxOutputBytes,
	})
	if result.TimedOut || ctx.Err() != nil {
		docker.removeContainer(invocation.cidFile)
	}
	result = maskHostPaths(maskSecrets(result, command.Environment), docker.mounts)
	return result, executeErr
}

type dockerInvocation struct {
	arguments []string
	envFile   string
	cidFile   string
}

func (invocation dockerInvocation) cleanup() {
	if invocation.envFile != "" {
		_ = os.Remove(invocation.envFile)
	}
	if invocation.cidFile != "" {
		_ = os.Remove(invocation.cidFile)
	}
}

func (docker *Docker) buildInvocation(command Command) (dockerInvocation, error) {
	workingDirectory, err := cleanContainerWorkingDirectory(command.WorkingDirectory, docker.mounts)
	if err != nil {
		return dockerInvocation{}, err
	}
	environment := mergeEnvironment(map[string]string{
		"HOME": "/mnt/user-data/workspace", "GOFER_SANDBOX": "docker",
	}, command.Environment)
	envFile, err := writeEnvironmentFile(environment)
	if err != nil {
		return dockerInvocation{}, err
	}
	cidFile, err := uniqueTemporaryPath("gofer-sandbox-cid-")
	if err != nil {
		_ = os.Remove(envFile)
		return dockerInvocation{}, err
	}
	invocation := dockerInvocation{envFile: envFile, cidFile: cidFile}
	invocation.arguments = docker.baseArguments(workingDirectory, envFile, cidFile)
	invocation.arguments = append(invocation.arguments, docker.image, "/bin/sh", "-lc", command.Script)
	return invocation, nil
}

func (docker *Docker) baseArguments(workingDirectory, envFile, cidFile string) []string {
	arguments := []string{
		"run", "--rm", "--init", "--cidfile", cidFile,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(docker.pidsLimit), "--memory", docker.memory,
		"--cpus", strconv.FormatFloat(docker.cpus, 'f', -1, 64),
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=" + strconv.FormatInt(docker.tmpfsSizeBytes, 10),
		"--workdir", workingDirectory, "--env-file", envFile,
	}
	if !docker.networkEnabled {
		arguments = append(arguments, "--network", "none")
	}
	if docker.user != "" {
		arguments = append(arguments, "--user", docker.user)
	}
	for _, mount := range docker.mounts {
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		arguments = append(arguments, "--volume", mount.Source+":"+mount.Target+":"+mode)
	}
	return arguments
}

func applyDockerDefaults(config *DockerConfig) {
	if config.User == "" {
		config.User = currentUserSpec()
	}
	if config.Memory == "" {
		config.Memory = "1g"
	}
	if config.CPUs == 0 {
		config.CPUs = 2
	}
	if config.PIDsLimit == 0 {
		config.PIDsLimit = 256
	}
	if config.TmpfsSizeBytes == 0 {
		config.TmpfsSizeBytes = 256 << 20
	}
}

func validateDockerResources(config DockerConfig) error {
	if !memoryPattern.MatchString(config.Memory) || config.CPUs <= 0 || config.CPUs > 256 ||
		config.PIDsLimit < 16 || config.PIDsLimit > 1_000_000 ||
		config.TmpfsSizeBytes < 1<<20 || config.TmpfsSizeBytes > 1<<40 ||
		strings.ContainsAny(config.User, "\x00\r\n") {
		return fmt.Errorf("%w: invalid Docker resource limit", ErrInvalidConfig)
	}
	return nil
}

func writeEnvironmentFile(environment map[string]string) (string, error) {
	for name, value := range environment {
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("%w: Docker environment value %q contains a newline", ErrInvalidCommand, name)
		}
	}
	file, err := os.CreateTemp("", "gofer-sandbox-env-")
	if err != nil {
		return "", err
	}
	name := file.Name()
	writeErr := file.Chmod(0o600)
	if writeErr == nil {
		_, writeErr = file.WriteString(strings.Join(environmentList(environment), "\n") + "\n")
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return "", errors.Join(writeErr, closeErr)
	}
	return name, nil
}

func uniqueTemporaryPath(pattern string) (string, error) {
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func (docker *Docker) removeContainer(cidFile string) {
	data, err := os.ReadFile(cidFile)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, docker.binary, "rm", "--force", strings.TrimSpace(string(data)))
	command.Stdout, command.Stderr = nil, nil
	_ = command.Run()
}

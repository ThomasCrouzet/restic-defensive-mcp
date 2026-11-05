package testrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Disposable backend services are pinned by OCI manifest digest. Docker verifies
// the digest during pull, which binds the test to the reviewed image content.
const (
	RESTServerVersion = "0.14.0"
	RESTServerImage   = "docker.io/restic/rest-server@sha256:d2aff06f47eb38637dff580c3e6bce4af98f386c396a25d32eb6727ec96214a5"

	S3GatewayVersion = "v1.10.0"
	S3GatewayImage   = "docker.io/versity/versitygw@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"

	// Synthetic credentials are intentionally low-entropy test values. They are
	// never accepted from a caller and never configured for a real service.
	SyntheticRESTUsername = "restuser"
	SyntheticRESTPassword = "synthetic-rest-password"

	SyntheticS3AccessKey = "synthetic-access-key"
	SyntheticS3SecretKey = "synthetic-secret-key"
	SyntheticS3Region    = "us-east-1"

	// B2 has no local service in this suite. These values are used only to prove
	// that configuration loading and credential redaction work without network
	// access; they are never sent to Backblaze.
	SyntheticB2Location   = "b2:restic-defensive-mcp-synthetic/test-repository"
	SyntheticB2AccountID  = "synthetic-b2-account-id"
	SyntheticB2AccountKey = "synthetic-b2-account-key"

	serviceStartTimeout = 60 * time.Second
	serviceStopTimeout  = 15 * time.Second
)

// RemoteService is one disposable Docker-hosted repository backend.
type RemoteService struct {
	Kind      string
	Version   string
	Image     string
	Endpoint  string
	DataDir   string
	Bucket    string
	Container string
}

// StartREST starts a restic REST server with htpasswd authentication.
func StartREST(ctx context.Context, root string) (*RemoteService, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("remote backend containers are not supported on Windows")
	}
	if err := pullImage(ctx, RESTServerImage); err != nil {
		return nil, err
	}
	if err := verifyToolVersion(ctx, RESTServerImage, "/usr/bin/rest-server", RESTServerVersion); err != nil {
		return nil, err
	}
	dataDir := filepath.Join(root, "rest-service")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	passwordHash, err := hashRESTPassword(ctx)
	if err != nil {
		return nil, err
	}
	passwordFile, err := writeSecretTemp(root, "rest-htpasswd-", passwordHash+"\n")
	if err != nil {
		return nil, err
	}
	container, err := newContainerName("rest")
	if err != nil {
		return nil, err
	}
	service := &RemoteService{
		Kind:      "rest",
		Version:   RESTServerVersion,
		Image:     RESTServerImage,
		DataDir:   dataDir,
		Container: container,
	}
	args := []string{
		"run", "--detach", "--rm", "--name", container,
		"--publish", "127.0.0.1::8000",
	}
	if user := containerUser(); user != "" {
		args = append(args, "--user", user)
	}
	args = append(args,
		"--volume", dataDir+":/data",
		"--volume", passwordFile+":/htpasswd:ro",
		"--entrypoint", "/usr/bin/rest-server",
		RESTServerImage,
		"--path", "/data",
		"--listen", ":8000",
		"--htpasswd-file", "/htpasswd",
	)
	if _, err := dockerOutput(ctx, args...); err != nil {
		_ = service.Stop(context.Background())
		return nil, err
	}
	endpoint, err := service.waitEndpoint(ctx, 8000)
	if err != nil {
		_ = service.Stop(context.Background())
		return nil, err
	}
	service.Endpoint = endpoint
	return service, nil
}

// StartS3 starts a Versity S3 Gateway backed by a disposable POSIX directory.
// A directory under the gateway root represents the pre-created bucket.
func StartS3(ctx context.Context, root, bucket string) (*RemoteService, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("remote backend containers are not supported on Windows")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("s3 service requires a bucket name")
	}
	if err := pullImage(ctx, S3GatewayImage); err != nil {
		return nil, err
	}
	if err := verifyToolVersion(ctx, S3GatewayImage, "", S3GatewayVersion); err != nil {
		return nil, err
	}
	dataDir := filepath.Join(root, "s3-service")
	if err := os.MkdirAll(filepath.Join(dataDir, bucket), 0o700); err != nil {
		return nil, err
	}
	container, err := newContainerName("s3")
	if err != nil {
		return nil, err
	}
	service := &RemoteService{
		Kind:      "s3",
		Version:   S3GatewayVersion,
		Image:     S3GatewayImage,
		DataDir:   dataDir,
		Bucket:    bucket,
		Container: container,
	}
	args := []string{
		"run", "--detach", "--rm", "--name", container,
		"--publish", "127.0.0.1::7070",
	}
	if user := containerUser(); user != "" {
		args = append(args, "--user", user)
	}
	args = append(args,
		"--volume", dataDir+":/data",
		S3GatewayImage,
		"--port", ":7070",
		"--access", SyntheticS3AccessKey,
		"--secret", SyntheticS3SecretKey,
		"posix", "/data",
	)
	if _, err := dockerOutput(ctx, args...); err != nil {
		_ = service.Stop(context.Background())
		return nil, err
	}
	endpoint, err := service.waitEndpoint(ctx, 7070)
	if err != nil {
		_ = service.Stop(context.Background())
		return nil, err
	}
	service.Endpoint = endpoint
	return service, nil
}

// RepositoryURL returns a restic location for a repository prefix.
func (s *RemoteService) RepositoryURL(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	switch s.Kind {
	case "s3":
		return fmt.Sprintf("s3:http://%s/%s/%s", s.Endpoint, s.Bucket, prefix)
	default:
		return fmt.Sprintf("rest:http://%s/%s/", s.Endpoint, prefix)
	}
}

// RepositoryDataDir returns the host path that backs one repository prefix.
func (s *RemoteService) RepositoryDataDir(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if s.Kind == "s3" {
		return filepath.Join(s.DataDir, s.Bucket, prefix)
	}
	return filepath.Join(s.DataDir, prefix)
}

// LockDir returns the host path of the repository lock directory.
func (s *RemoteService) LockDir(prefix string) string {
	return filepath.Join(s.RepositoryDataDir(prefix), "locks")
}

// RESTEnv returns synthetic REST credentials for the test-only fixture.
func (s *RemoteService) RESTEnv() []string {
	return []string{
		"RESTIC_REST_USERNAME=" + SyntheticRESTUsername,
		"RESTIC_REST_PASSWORD=" + SyntheticRESTPassword,
	}
}

// S3Env returns synthetic S3 credentials for the test-only fixture.
func (s *RemoteService) S3Env() []string {
	return []string{
		"AWS_ACCESS_KEY_ID=" + SyntheticS3AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + SyntheticS3SecretKey,
		"AWS_REGION=" + SyntheticS3Region,
		"AWS_DEFAULT_REGION=" + SyntheticS3Region,
	}
}

// SyntheticB2Env returns B2 credential keys without contacting Backblaze.
func SyntheticB2Env() []string {
	return []string{
		"B2_ACCOUNT_ID=" + SyntheticB2AccountID,
		"B2_ACCOUNT_KEY=" + SyntheticB2AccountKey,
	}
}

// S3EnvWithSecret returns S3 credentials with an explicitly wrong secret key.
func (s *RemoteService) S3EnvWithSecret(secret string) []string {
	return []string{
		"AWS_ACCESS_KEY_ID=" + SyntheticS3AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + secret,
		"AWS_REGION=" + SyntheticS3Region,
		"AWS_DEFAULT_REGION=" + SyntheticS3Region,
	}
}

// Stop force-removes the container and waits for Docker to forget it.
func (s *RemoteService) Stop(ctx context.Context) error {
	if s == nil || s.Container == "" {
		return nil
	}
	_, _ = dockerOutput(ctx, "rm", "--force", s.Container)
	deadline := time.Now().Add(serviceStopTimeout)
	for time.Now().Before(deadline) {
		if s.ContainerGone(ctx) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("container %s was not removed before the deadline", s.Container)
}

// ContainerGone reports whether Docker no longer knows the container.
func (s *RemoteService) ContainerGone(ctx context.Context) bool {
	if s == nil || s.Container == "" {
		return true
	}
	out, err := dockerOutput(ctx, "inspect", "--format", "{{.Id}}", s.Container)
	if err != nil {
		return true
	}
	return strings.TrimSpace(out) == ""
}

func (s *RemoteService) waitEndpoint(ctx context.Context, containerPort int) (string, error) {
	deadline := time.Now().Add(serviceStartTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		out, err := dockerOutput(ctx, "port", s.Container, fmt.Sprintf("%d/tcp", containerPort))
		if err == nil {
			host, port, splitErr := net.SplitHostPort(strings.TrimSpace(strings.Split(out, "\n")[0]))
			if splitErr == nil && host != "" && port != "" {
				endpoint := net.JoinHostPort(host, port)
				if endpointReachable(ctx, endpoint) {
					return endpoint, nil
				}
			}
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("endpoint did not become reachable")
	}
	return "", fmt.Errorf("service %s failed to start: %w", s.Kind, lastErr)
}

func endpointReachable(ctx context.Context, endpoint string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+"/", nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return true
}

func pullImage(ctx context.Context, image string) error {
	if _, err := dockerOutput(ctx, "pull", image); err != nil {
		return fmt.Errorf("pull pinned backend image: %w", err)
	}
	return nil
}

func verifyToolVersion(ctx context.Context, image, entrypoint, expected string) error {
	args := []string{"run", "--rm"}
	if entrypoint != "" {
		args = append(args, "--entrypoint", entrypoint)
	}
	args = append(args, image, "--version")
	out, err := dockerOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("read embedded service version: %w", err)
	}
	if !strings.Contains(out, expected) {
		return fmt.Errorf("service image version %q does not contain %q", strings.TrimSpace(out), expected)
	}
	return nil
}

func hashRESTPassword(ctx context.Context) (string, error) {
	out, err := dockerOutput(ctx, "run", "--rm", "--entrypoint", "htpasswd",
		RESTServerImage, "-nbBC", "10", SyntheticRESTUsername, SyntheticRESTPassword)
	if err != nil {
		return "", fmt.Errorf("hash synthetic REST password: %w", err)
	}
	hash := strings.TrimSpace(out)
	if !strings.HasPrefix(hash, SyntheticRESTUsername+":$2") {
		return "", fmt.Errorf("htpasswd returned an unexpected entry")
	}
	return hash, nil
}

func newContainerName(kind string) (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "restic-defensive-" + kind + "-" + hex.EncodeToString(raw[:]), nil
}

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Do not echo argv: S3 credentials appear there in synthetic test runs.
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("docker %s failed: %s", args[0], message)
	}
	return stdout.String(), nil
}

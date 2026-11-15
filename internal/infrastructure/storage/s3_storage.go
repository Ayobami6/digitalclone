// Package storage implements domain.AssetStorage for S3-compatible object buckets
// (AWS S3 / Cloudflare R2 / GCS interoperability) with local static HTTP fallback.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

// Config configures S3-compatible uploads and local asset caching/HTTP fallback.
type Config struct {
	LocalDir        string
	LocalBaseURL    string
	S3Bucket        string
	S3Region        string
	S3Endpoint      string
	S3AccessKeyID   string
	S3SecretKey     string
	S3PublicBaseURL string
	HTTPClient      *http.Client
}

// HybridAssetStorage writes generated media locally (for direct YouTube streaming and local HTTP serving)
// and uploads to S3-compatible storage when credentials are configured.
type HybridAssetStorage struct {
	config     Config
	httpClient *http.Client
	breaker    *resilience.CircuitBreaker
}

// NewHybridAssetStorage constructs a HybridAssetStorage adapter.
func NewHybridAssetStorage(config Config) *HybridAssetStorage {
	if strings.TrimSpace(config.LocalDir) == "" {
		config.LocalDir = "./assets"
	}
	if strings.TrimSpace(config.LocalBaseURL) == "" {
		config.LocalBaseURL = "https://localhost:8080"
	}
	config.LocalBaseURL = strings.TrimRight(config.LocalBaseURL, "/")
	config.S3PublicBaseURL = strings.TrimRight(config.S3PublicBaseURL, "/")
	if strings.TrimSpace(config.S3Region) == "" {
		config.S3Region = "us-east-1"
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	return &HybridAssetStorage{
		config:     config,
		httpClient: httpClient,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "s3-asset-storage",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// UploadAsset saves bytes locally and uploads to S3/R2 when configured, returning both paths.
func (storage *HybridAssetStorage) UploadAsset(
	ctx context.Context,
	batchID string,
	platform domain.Platform,
	mediaType domain.MediaType,
	fileName string,
	mimeType string,
	data []byte,
) (*domain.MediaAsset, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: asset payload cannot be empty", domain.ErrValidationFailed)
	}
	cleanName := filepath.Base(strings.TrimSpace(fileName))
	if cleanName == "." || cleanName == "/" || cleanName == "" {
		cleanName = uuid.NewString()
	}

	if err := os.MkdirAll(storage.config.LocalDir, 0o750); err != nil {
		return nil, fmt.Errorf("create local asset directory: %w", err)
	}
	localPath := filepath.Join(storage.config.LocalDir, cleanName)
	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write local asset file: %w", err)
	}

	publicURL := fmt.Sprintf("%s/assets/%s", storage.config.LocalBaseURL, url.PathEscape(cleanName))

	if storage.hasS3Config() {
		objectKey := fmt.Sprintf("digitalclone/%s/%s/%s", batchID, platform, cleanName)
		uploadedURL, err := storage.putObjectSigV4(ctx, objectKey, mimeType, data)
		if err != nil {
			return nil, fmt.Errorf("upload asset to S3: %w", err)
		}
		publicURL = uploadedURL
	}

	return &domain.MediaAsset{
		ID:        uuid.NewString(),
		MediaType: mediaType,
		MimeType:  mimeType,
		LocalPath: localPath,
		PublicURL: publicURL,
		SizeBytes: int64(len(data)),
	}, nil
}

func (storage *HybridAssetStorage) hasS3Config() bool {
	return strings.TrimSpace(storage.config.S3Bucket) != "" &&
		strings.TrimSpace(storage.config.S3AccessKeyID) != "" &&
		strings.TrimSpace(storage.config.S3SecretKey) != ""
}

func (storage *HybridAssetStorage) putObjectSigV4(ctx context.Context, objectKey, mimeType string, data []byte) (string, error) {
	endpoint := strings.TrimRight(storage.config.S3Endpoint, "/")
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", storage.config.S3Region)
	}
	targetURL := fmt.Sprintf("%s/%s/%s", endpoint, storage.config.S3Bucket, strings.TrimLeft(objectKey, "/"))

	var publicURL string
	err := storage.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("build S3 PUT request: %w", err)
		}
		if mimeType != "" {
			req.Header.Set("Content-Type", mimeType)
		}
		signRequestSigV4(req, storage.config.S3AccessKeyID, storage.config.S3SecretKey, storage.config.S3Region, data, time.Now().UTC())

		resp, err := storage.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("execute S3 PUT request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("S3 upload returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		}

		if storage.config.S3PublicBaseURL != "" {
			publicURL = fmt.Sprintf("%s/%s", storage.config.S3PublicBaseURL, strings.TrimLeft(objectKey, "/"))
		} else {
			publicURL = targetURL
		}
		return nil
	})
	return publicURL, err
}

// signRequestSigV4 computes an AWS Signature Version 4 Authorization header for S3/R2 PUT requests.
func signRequestSigV4(req *http.Request, accessKey, secretKey, region string, payload []byte, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	payloadHash := sha256Hex(payload)

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.URL.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

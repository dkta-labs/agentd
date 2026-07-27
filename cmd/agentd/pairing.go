package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/mdp/qrterminal/v3"

	"github.com/dkta-labs/agentd/internal/store"
)

func buildPairingURL(publicURL, enrollmentToken string) (string, error) {
	if strings.TrimSpace(publicURL) == "" {
		return "", errors.New("publicUrl is required to generate a pairing QR code")
	}
	if enrollmentToken == "" {
		return "", errors.New("AGENTD_ENROLLMENT_TOKEN is required to generate a pairing QR code")
	}
	target, err := url.Parse(publicURL)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil ||
		(target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" {
		return "", errors.New("publicUrl must be an HTTPS origin without credentials, path, query, or fragment")
	}
	target.Path = ""
	target.Fragment = url.Values{"enroll": {enrollmentToken}}.Encode()
	return target.String(), nil
}

func printPairingQR(output io.Writer, publicURL, enrollmentToken string) error {
	pairingURL, err := buildPairingURL(publicURL, enrollmentToken)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Scan this one-time pairing code with the Android camera:"); err != nil {
		return err
	}
	qrterminal.GenerateWithConfig(pairingURL, qrterminal.Config{
		Level:      qrterminal.M,
		Writer:     output,
		HalfBlocks: true,
		QuietZone:  4,
	})
	_, err = fmt.Fprintln(output, "The code expires when one device finishes enrollment.")
	return err
}

func resolvePairingConfigPath(explicitPath string) (string, error) {
	if explicitPath != "" {
		return explicitPath, nil
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	path := filepath.Join(configHome, "agentd", "agentd.json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("find default agentd config %q: %w; use --config to select another file", path, err)
	}
	return path, nil
}

func enrollmentTokenForPairing(configPath, environmentToken string) (string, error) {
	if environmentToken != "" {
		return environmentToken, nil
	}
	envPath := filepath.Join(filepath.Dir(configPath), "agentd.env")
	token, err := readEnvSecret(envPath, "AGENTD_ENROLLMENT_TOKEN")
	if err != nil {
		return "", fmt.Errorf("load pairing credential: %w", err)
	}
	return token, nil
}

func readEnvSecret(path, name string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s permissions %04o expose secrets; require 0600", path, info.Mode().Perm())
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != name {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if strings.HasPrefix(value, "\"") {
			value, err = strconv.Unquote(value)
			if err != nil {
				return "", fmt.Errorf("decode %s in %s: %w", name, path, err)
			}
		}
		if value == "" {
			return "", fmt.Errorf("%s is empty in %s", name, path)
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s is missing from %s", name, path)
}

func ensurePairingTokenUnused(ctx context.Context, dataDir, enrollmentToken string) error {
	digest := sha256.Sum256([]byte(enrollmentToken))
	used, err := store.EnrollmentTokenUsedAt(ctx, dataDir, hex.EncodeToString(digest[:]))
	if err != nil {
		return err
	}
	if used {
		return errors.New("the configured enrollment token has already been used; generate a new token, update agentd.env, and restart agentd before pairing another device")
	}
	return nil
}

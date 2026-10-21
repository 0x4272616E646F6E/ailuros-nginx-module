// Package repo holds helpers for checking the repository's own metadata.
package repo

import (
	"encoding/json"
	"fmt"
	"os"
)

// Version is one supported NGF release and the NGINX source it is built from.
type Version struct {
	NGF         string `json:"ngf"`
	NGINX       string `json:"nginx"`
	NGINXSHA256 string `json:"nginx_sha256"`
}

// LoadVersions reads the versions file at path.
func LoadVersions(path string) ([]Version, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read versions: %w", err)
	}
	var versions []Version
	if err := json.Unmarshal(data, &versions); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return versions, nil
}

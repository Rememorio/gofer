package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	skillNamePattern  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type frontmatter struct {
	Name              string           `yaml:"name"`
	Description       string           `yaml:"description"`
	License           string           `yaml:"license"`
	AllowedTools      *[]string        `yaml:"allowed-tools"`
	RequiredSecrets   []secretMetadata `yaml:"required-secrets"`
	SecretsAutonomous *bool            `yaml:"secrets-autonomous"`
	Metadata          map[string]any   `yaml:"metadata"`
	Compatibility     string           `yaml:"compatibility"`
	Version           string           `yaml:"version"`
	Author            string           `yaml:"author"`
}

type secretMetadata struct {
	Name     string
	Optional bool
}

func (secret *secretMetadata) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return errors.New("secret requirement must be a string or mapping")
		}
		secret.Name = node.Value
		return nil
	case yaml.MappingNode:
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Value != "name" && key.Value != "optional" {
				return fmt.Errorf("unknown secret requirement field %q", key.Value)
			}
		}
		var value struct {
			Name     string `yaml:"name"`
			Optional bool   `yaml:"optional"`
		}
		if err := node.Decode(&value); err != nil {
			return err
		}
		secret.Name, secret.Optional = value.Name, value.Optional
		return nil
	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
		return errors.New("secret requirement must be a string or mapping")
	default:
		return errors.New("secret requirement must be a string or mapping")
	}
}

func parseFile(
	ctx context.Context,
	filename string,
	category Category,
	relativePath string,
	virtualRoot string,
	maxBytes int64,
) (Skill, string, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, "", err
	}
	file, err := os.Open(filename)
	if err != nil {
		return Skill{}, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return Skill{}, "", errors.Join(readErr, closeErr)
	}
	if int64(len(data)) > maxBytes {
		return Skill{}, "", fmt.Errorf("%w: SKILL.md exceeds %d bytes", ErrInvalidSkill, maxBytes)
	}
	metadataText, body, err := splitDocument(data)
	if err != nil {
		return Skill{}, "", err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(metadataText))
	decoder.KnownFields(true)
	var metadata frontmatter
	if err := decoder.Decode(&metadata); err != nil {
		return Skill{}, "", fmt.Errorf("%w: decode frontmatter: %w", ErrInvalidSkill, err)
	}
	if err := ensureSingleYAMLDocument(decoder); err != nil {
		return Skill{}, "", err
	}
	skill, err := validateMetadata(metadata, category, relativePath, virtualRoot)
	if err != nil {
		return Skill{}, "", err
	}
	if strings.TrimSpace(string(body)) == "" {
		return Skill{}, "", fmt.Errorf("%w: instruction body is empty", ErrInvalidSkill)
	}
	return skill, string(data), nil
}

func splitDocument(data []byte) ([]byte, []byte, error) {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return nil, nil, fmt.Errorf("%w: YAML frontmatter is required", ErrInvalidSkill)
	}
	closing := bytes.Index(normalized[4:], []byte("\n---\n"))
	if closing < 0 {
		return nil, nil, fmt.Errorf("%w: YAML frontmatter is not closed", ErrInvalidSkill)
	}
	closing += 4
	return normalized[4:closing], normalized[closing+5:], nil
}

func validateMetadata(metadata frontmatter, category Category, relativePath, virtualRoot string) (Skill, error) {
	name := strings.TrimSpace(metadata.Name)
	description := strings.TrimSpace(metadata.Description)
	if !skillNamePattern.MatchString(name) || len(name) > 64 {
		return Skill{}, fmt.Errorf("%w: name must be hyphen-case and at most 64 characters", ErrInvalidSkill)
	}
	if description == "" || len(description) > 1024 || strings.ContainsAny(description, "<>") {
		return Skill{}, fmt.Errorf("%w: description must be 1-1024 characters without angle brackets", ErrInvalidSkill)
	}
	if !category.valid() || !validRelativePath(relativePath) {
		return Skill{}, fmt.Errorf("%w: category or relative path is invalid", ErrInvalidSkill)
	}
	allowedTools, allowedSet, err := validateAllowedTools(metadata.AllowedTools)
	if err != nil {
		return Skill{}, err
	}
	secrets, err := validateSecrets(metadata.RequiredSecrets)
	if err != nil {
		return Skill{}, err
	}
	secretsAutonomous := true
	if metadata.SecretsAutonomous != nil {
		secretsAutonomous = *metadata.SecretsAutonomous
	}
	documentPath := path.Join(virtualRoot, string(category), relativePath, "SKILL.md")
	return Skill{
		Name: name, Description: description, License: strings.TrimSpace(metadata.License),
		Category: category, RelativePath: relativePath, DocumentPath: documentPath,
		AllowedTools: allowedTools, AllowedToolsSet: allowedSet, RequiredSecrets: secrets,
		SecretsAutonomous: secretsAutonomous, Enabled: true,
		Compatibility: strings.TrimSpace(metadata.Compatibility), Version: strings.TrimSpace(metadata.Version),
		Author: strings.TrimSpace(metadata.Author),
	}, nil
}

func validateAllowedTools(values *[]string) ([]string, bool, error) {
	if values == nil {
		return nil, false, nil
	}
	result := make([]string, 0, len(*values))
	seen := make(map[string]struct{}, len(*values))
	for _, value := range *values {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, false, fmt.Errorf("%w: allowed-tools contains an empty name", ErrInvalidSkill)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, false, fmt.Errorf("%w: allowed-tools contains duplicate %q", ErrInvalidSkill, name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, true, nil
}

func validateSecrets(values []secretMetadata) ([]SecretRequirement, error) {
	secrets := make([]SecretRequirement, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value.Name)
		if !secretNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: invalid required secret name %q", ErrInvalidSkill, name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate required secret %q", ErrInvalidSkill, name)
		}
		seen[name] = struct{}{}
		secrets = append(secrets, SecretRequirement{Name: name, Optional: value.Optional})
	}
	return secrets, nil
}

func ensureSingleYAMLDocument(decoder *yaml.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: decode trailing frontmatter: %w", ErrInvalidSkill, err)
	}
	return fmt.Errorf("%w: multiple frontmatter documents", ErrInvalidSkill)
}

func validRelativePath(value string) bool {
	return value != "." && !strings.Contains(value, "\\") && fs.ValidPath(value)
}

func (category Category) valid() bool {
	switch category {
	case CategoryPublic, CategoryCustom, CategoryIntegration, CategoryLegacy:
		return true
	default:
		return false
	}
}

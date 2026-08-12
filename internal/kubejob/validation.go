package kubejob

import (
	"errors"
	"fmt"
	"strings"
)

func validateLabels(labels map[string]string) error {
	for key, value := range labels {
		if err := validateLabelKey(key); err != nil {
			return fmt.Errorf("label key %q: %w", key, err)
		}
		if err := validateLabelValue(value); err != nil {
			return fmt.Errorf("label %q value %q: %w", key, value, err)
		}
	}
	return nil
}

func validateLabelKey(value string) error {
	if value == "" {
		return errors.New("must be non-empty")
	}
	prefix, name, hasPrefix := strings.Cut(value, "/")
	if hasPrefix {
		if strings.Contains(name, "/") {
			return errors.New("must contain at most one slash")
		}
		if err := validateDNSSubdomain(prefix, "prefix", 253); err != nil {
			return err
		}
	} else {
		name = prefix
	}
	if len(name) > 63 {
		return errors.New("name must be at most 63 characters")
	}
	if !validLabelPart(name, false) {
		return errors.New("name must start and end with an alphanumeric character and contain only alphanumerics, '-', '_' or '.'")
	}
	return nil
}

func validateLabelValue(value string) error {
	if len(value) > 63 {
		return errors.New("must be at most 63 characters")
	}
	if !validLabelPart(value, true) {
		return errors.New("must be empty or start and end with an alphanumeric character and contain only alphanumerics, '-', '_' or '.'")
	}
	return nil
}

func validLabelPart(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if !asciiAlphaNumeric(value[0]) || !asciiAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := 1; index < len(value)-1; index++ {
		char := value[index]
		if !asciiAlphaNumeric(char) && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func validateDNSLabel(value, field string) error {
	return validateDNSSubdomain(value, field, 63)
}

func validateDNSSubdomain(value, field string, maxLength int) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > maxLength {
		return fmt.Errorf("%s must be at most %d characters", field, maxLength)
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" || len(part) > 63 || !asciiLowerNumeric(part[0]) || !asciiLowerNumeric(part[len(part)-1]) {
			return fmt.Errorf("%s must be a lowercase DNS name", field)
		}
		for index := 1; index < len(part)-1; index++ {
			if char := part[index]; !asciiLowerNumeric(char) && char != '-' {
				return fmt.Errorf("%s must be a lowercase DNS name", field)
			}
		}
	}
	return nil
}

func validateSecretKey(value string) error {
	if value == "" {
		return errors.New("API token secret key is required")
	}
	if len(value) > 253 {
		return errors.New("API token secret key must be at most 253 characters")
	}
	for index := range value {
		char := value[index]
		if !asciiAlphaNumeric(char) && char != '-' && char != '_' && char != '.' {
			return errors.New("API token secret key contains an unsupported character")
		}
	}
	return nil
}

func asciiAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func asciiLowerNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

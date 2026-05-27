package domain

import (
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"
)

// slugPattern matches lowercase, hyphen-separated slugs.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Validator wraps go-playground/validator (ADR-005) behind a small interface so
// the service layer does not depend directly on the tag engine.
type Validator struct {
	v *validator.Validate
}

// NewValidator builds a Validator with WPMgr custom rules registered.
func NewValidator() *Validator {
	v := validator.New(validator.WithRequiredStructEnabled())
	_ = v.RegisterValidation("slug", func(fl validator.FieldLevel) bool {
		return slugPattern.MatchString(fl.Field().String())
	})
	return &Validator{v: v}
}

// Struct validates a struct by its `validate` tags, returning a KindValidation
// domain error listing the offending fields, or nil if valid.
func (val *Validator) Struct(s any) error {
	err := val.v.Struct(s)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if ok := asValidationErrors(err, &verrs); !ok {
		return Internal("validation_error", "validation failed").WithCause(err)
	}
	fields := make([]string, 0, len(verrs))
	for _, fe := range verrs {
		fields = append(fields, fe.Field())
	}
	return Validation("validation_failed", "invalid request: "+strings.Join(fields, ", ")).WithCause(err)
}

func asValidationErrors(err error, target *validator.ValidationErrors) bool {
	if ve, ok := err.(validator.ValidationErrors); ok {
		*target = ve
		return true
	}
	return false
}

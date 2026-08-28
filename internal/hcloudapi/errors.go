// Package hcloudapi wraps the Hetzner Cloud API. It is the only package that
// imports hcloud-go, so the rest of the provider deals in our own types.
package hcloudapi

import (
	"errors"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Class is how the provider should react to an error from the Hetzner API.
// Getting this mapping right is the difference between a stockout that routes
// around itself and one that hot-loops on an unorderable server type.
type Class int

const (
	// ClassNone means no error.
	ClassNone Class = iota

	// ClassCapacity means this particular server type is not obtainable in
	// this particular datacenter right now. Mark that (type, datacenter)
	// offering unavailable and fall through to the next-cheapest candidate.
	ClassCapacity

	// ClassQuota means the account or project is at a limit. A different
	// server type will not help, so suppress all offerings for the cooldown
	// and surface it loudly: this needs an operator, not a retry.
	ClassQuota

	// ClassTransient means try again unchanged. It must NOT mark any offering
	// unavailable: rate limiting is caused by our own request volume, and
	// letting it poison the catalog turns a slowdown into a self-inflicted
	// capacity outage.
	ClassTransient

	// ClassConfig means the request will never succeed as written. Surface it
	// on the NodeClass rather than retrying.
	ClassConfig

	// ClassFatal means the credential is wrong or lacks permission. Retrying
	// cannot fix it and every attempt burns rate limit.
	ClassFatal
)

func (c Class) String() string {
	switch c {
	case ClassNone:
		return "none"
	case ClassCapacity:
		return "capacity"
	case ClassQuota:
		return "quota"
	case ClassTransient:
		return "transient"
	case ClassConfig:
		return "config"
	case ClassFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// classes maps Hetzner error codes to a reaction. Codes absent here are
// treated as transient, see Classify.
var classes = map[string]Class{
	// Capacity. The server type cannot be placed here, now.
	string(hcloud.ErrorCodeResourceUnavailable): ClassCapacity,
	// placement_error covers both "no host has room" and "the spread placement
	// group is full", which are indistinguishable from the outside. No amount
	// of falling back to another server type fixes a full group.
	string(hcloud.ErrorCodePlacementError): ClassCapacity,
	// Documented as volume space, so it will not fire on a server create, but
	// it is still a capacity condition.
	string(hcloud.ErrorCodeNoSpaceLeftInLocation): ClassCapacity,
	// Datacenter-wide rather than type-specific; the caller should suppress
	// the whole datacenter rather than one offering.
	string(hcloud.ErrorCodeMaintenance): ClassCapacity,

	// Quota.
	string(hcloud.ErrorCodeResourceLimitExceeded): ClassQuota,

	// Transient.
	string(hcloud.ErrorCodeRateLimitExceeded): ClassTransient,
	string(hcloud.ErrorCodeServiceError):      ClassTransient,
	string(hcloud.ErrorCodeServerError):       ClassTransient,
	string(hcloud.ErrorCodeBadGateway):        ClassTransient,
	string(hcloud.ErrorCodeTimeout):           ClassTransient,
	string(hcloud.ErrorCodeConflict):          ClassTransient,
	string(hcloud.ErrorCodeLocked):            ClassTransient,
	string(hcloud.ErrorCodeResourceLocked):    ClassTransient,
	string(hcloud.ErrorCodeProtected):         ClassTransient,
	string(hcloud.ErrorCodeJSONError):         ClassTransient,
	string(hcloud.ErrorCodeRobotUnavailable):  ClassTransient,
	string(hcloud.ErrorCodeUnknownError):      ClassTransient,

	// Config.
	string(hcloud.ErrorCodeInvalidInput):          ClassConfig,
	string(hcloud.ErrorCodeNotFound):              ClassConfig,
	string(hcloud.ErrorUnsupportedError):          ClassConfig,
	string(hcloud.ErrorCodeInvalidServerType):     ClassConfig,
	string(hcloud.ErrorCodeUniquenessError):       ClassConfig,
	string(hcloud.ErrorCodeNetworksOverlap):       ClassConfig,
	string(hcloud.ErrorCodeServerAlreadyAttached): ClassConfig,

	// Fatal.
	string(hcloud.ErrorCodeForbidden):     ClassFatal,
	string(hcloud.ErrorCodeUnauthorized):  ClassFatal,
	string(hcloud.ErrorCodeTokenReadonly): ClassFatal,
}

// CodeUniqueness is the code Hetzner returns for a server name already in use.
// The create path singles it out: there it means a previous attempt's response
// was lost while the server was made, which is recoverable by adoption, unlike
// the rest of ClassConfig.
const CodeUniqueness = string(hcloud.ErrorCodeUniquenessError)

// Code extracts the Hetzner error code from err, following wrapping.
//
// Deliberately not hcloud.IsError, which only ever matches hcloud.Error.
// Server creation is asynchronous, and a placement failure surfaces later as
// hcloud.ActionError, a DIFFERENT type whose Code is a plain string rather
// than an hcloud.ErrorCode. So IsError is false for exactly the failure we
// care most about, and a provider relying on it retries the same unorderable
// server type forever.
func Code(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	// Synchronous HTTP errors.
	var apiErr hcloud.Error
	if errors.As(err, &apiErr) {
		return string(apiErr.Code), true
	}

	// Asynchronous action failures.
	var actionErr hcloud.ActionError
	if errors.As(err, &actionErr) {
		return actionErr.Code, true
	}

	return "", false
}

// Classify reports how the provider should react to err. Unrecognised codes,
// and non-Hetzner errors such as a dial timeout, are transient by design:
// guessing "capacity" would suppress an offering that is fine, and "config"
// would fail a NodeClaim that might have succeeded. Callers should count
// unrecognised codes so new ones become visible.
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	code, ok := Code(err)
	if !ok {
		return ClassTransient
	}
	if class, known := classes[code]; known {
		return class
	}
	return ClassTransient
}

// IsKnownCode reports whether Classify recognised the code, so callers can
// distinguish a deliberate transient classification from a fallback.
func IsKnownCode(err error) bool {
	code, ok := Code(err)
	if !ok {
		return false
	}
	_, known := classes[code]
	return known
}

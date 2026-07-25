package serviceproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	originalUserExtraPrefix  = "originaluser.open-cluster-management.io-"
	serviceAccountUserPrefix = "system:serviceaccount:"

	originalUserExtraUser   = originalUserExtraPrefix + "user"
	originalUserExtraGroups = originalUserExtraPrefix + "groups"
	originalUserExtraUID    = originalUserExtraPrefix + "uid"
	originalUserExtraExtra  = originalUserExtraPrefix + "extra"
)

type requestProcessingError struct {
	statusCode    int
	clientMessage string
	err           error
}

func (e *requestProcessingError) Error() string {
	return e.err.Error()
}

func (e *requestProcessingError) Unwrap() error {
	return e.err
}

func newRequestProcessingError(statusCode int, clientMessage string, err error) error {
	if err == nil {
		err = fmt.Errorf("%s", clientMessage)
	}
	return &requestProcessingError{
		statusCode:    statusCode,
		clientMessage: clientMessage,
		err:           err,
	}
}

func writeRequestProcessingError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	message := "Internal Server Error"
	var processingErr *requestProcessingError
	if errors.As(err, &processingErr) {
		statusCode = processingErr.statusCode
		message = processingErr.clientMessage
	}

	reason := metav1.StatusReasonInternalError
	switch statusCode {
	case http.StatusBadRequest:
		reason = metav1.StatusReasonBadRequest
	case http.StatusUnauthorized:
		reason = metav1.StatusReasonUnauthorized
	case http.StatusForbidden:
		reason = metav1.StatusReasonForbidden
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if encodeErr := json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Status",
		},
		Status:  metav1.StatusFailure,
		Message: message,
		Reason:  reason,
		Code:    int32(statusCode),
	}); encodeErr != nil {
		// The status code is already written, so there is nothing useful left to
		// send to the client.
		return
	}
}

// impersonationRequest contains only values parsed from client-supplied
// Impersonate-* headers. The original headers are removed before authentication,
// and only this validated representation may be applied to an outbound request.
type impersonationRequest struct {
	user   string
	groups []string
	uid    string
	extra  map[string][]string
}

func (r *impersonationRequest) clone() *impersonationRequest {
	if r == nil {
		return nil
	}

	cloned := &impersonationRequest{
		user:   r.user,
		groups: append([]string(nil), r.groups...),
		uid:    r.uid,
	}
	if len(r.extra) > 0 {
		cloned.extra = make(map[string][]string, len(r.extra))
		for key, values := range r.extra {
			cloned.extra[key] = append([]string(nil), values...)
		}
	}
	return cloned
}

// extractClientImpersonation removes all Impersonate-* headers, including
// unknown future variants, while building a trusted representation of the
// currently supported Kubernetes impersonation headers.
func extractClientImpersonation(headers http.Header) (*impersonationRequest, error) {
	var (
		found         bool
		userValues    []string
		uidValues     []string
		groupValues   []string
		extraValues   = map[string][]string{}
		inputError    error
		reservedExtra bool
	)

	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		if !strings.HasPrefix(lowerKey, "impersonate-") {
			continue
		}

		found = true
		delete(headers, key)

		switch {
		case lowerKey == strings.ToLower(authenticationv1.ImpersonateUserHeader):
			userValues = append(userValues, values...)
		case lowerKey == strings.ToLower(authenticationv1.ImpersonateGroupHeader):
			groupValues = append(groupValues, values...)
		case lowerKey == strings.ToLower(authenticationv1.ImpersonateUIDHeader):
			uidValues = append(uidValues, values...)
		case strings.HasPrefix(lowerKey, strings.ToLower(authenticationv1.ImpersonateUserExtraHeaderPrefix)):
			encodedKey := lowerKey[len(authenticationv1.ImpersonateUserExtraHeaderPrefix):]
			if encodedKey == "" {
				inputError = fmt.Errorf("impersonation extra header key must not be empty")
				continue
			}
			extraKey := unescapeExtraKey(encodedKey)
			if strings.HasPrefix(extraKey, originalUserExtraPrefix) {
				reservedExtra = true
				continue
			}
			extraValues[extraKey] = append(extraValues[extraKey], values...)
		default:
			inputError = fmt.Errorf("unknown impersonation header %q", key)
		}
	}

	if !found {
		return nil, nil
	}
	if inputError != nil {
		return nil, newRequestProcessingError(http.StatusBadRequest, inputError.Error(), inputError)
	}
	if reservedExtra {
		err := fmt.Errorf("impersonation extra keys with prefix %q are reserved", originalUserExtraPrefix)
		return nil, newRequestProcessingError(http.StatusForbidden, err.Error(), err)
	}
	if len(userValues) != 1 || userValues[0] == "" {
		err := fmt.Errorf("exactly one non-empty %s header is required", authenticationv1.ImpersonateUserHeader)
		return nil, newRequestProcessingError(http.StatusBadRequest, err.Error(), err)
	}
	if len(uidValues) > 1 || len(uidValues) == 1 && uidValues[0] == "" {
		err := fmt.Errorf("at most one non-empty %s header is allowed", authenticationv1.ImpersonateUIDHeader)
		return nil, newRequestProcessingError(http.StatusBadRequest, err.Error(), err)
	}

	requested := &impersonationRequest{
		user:   userValues[0],
		groups: groupValues,
		extra:  extraValues,
	}
	if len(uidValues) == 1 {
		requested.uid = uidValues[0]
	}
	if len(requested.extra) == 0 {
		requested.extra = nil
	}

	return requested, nil
}

func unescapeExtraKey(encodedKey string) string {
	key, err := url.PathUnescape(encodedKey)
	if err != nil {
		return encodedKey
	}
	return key
}

func escapeExtraKey(key string) string {
	var escaped strings.Builder
	for i := 0; i < len(key); i++ {
		if isLegalHeaderKeyByte(key[i]) && key[i] != '%' {
			escaped.WriteByte(key[i])
			continue
		}
		fmt.Fprintf(&escaped, "%%%02X", key[i])
	}
	return escaped.String()
}

func isLegalHeaderKeyByte(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(b))
}

func applyImpersonationHeaders(headers http.Header, requested *impersonationRequest) {
	stripClientImpersonationHeaders(headers)
	if requested == nil {
		return
	}

	headers.Set(authenticationv1.ImpersonateUserHeader, requested.user)
	if requested.uid != "" {
		headers.Set(authenticationv1.ImpersonateUIDHeader, requested.uid)
	}
	for _, group := range requested.groups {
		headers.Add(authenticationv1.ImpersonateGroupHeader, group)
	}

	extraKeys := make([]string, 0, len(requested.extra))
	for key := range requested.extra {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		header := authenticationv1.ImpersonateUserExtraHeaderPrefix + escapeExtraKey(key)
		for _, value := range requested.extra[key] {
			headers.Add(header, value)
		}
	}
}

func (s *serviceProxy) authorizeImpersonation(
	ctx context.Context,
	requester user.Info,
	requested *impersonationRequest,
) error {
	if requested == nil {
		return nil
	}

	attributes := make([]authorizationv1.ResourceAttributes, 0, 2+len(requested.groups)+len(requested.extra))
	if namespace, name, ok := splitServiceAccountUsername(requested.user); ok {
		attributes = append(attributes, authorizationv1.ResourceAttributes{
			Namespace: namespace,
			Verb:      "impersonate",
			Resource:  "serviceaccounts",
			Name:      name,
		})
	} else {
		attributes = append(attributes, authorizationv1.ResourceAttributes{
			Verb:     "impersonate",
			Resource: "users",
			Name:     requested.user,
		})
	}

	for _, group := range requested.groups {
		attributes = append(attributes, authorizationv1.ResourceAttributes{
			Verb:     "impersonate",
			Resource: "groups",
			Name:     group,
		})
	}
	if requested.uid != "" {
		attributes = append(attributes, authorizationv1.ResourceAttributes{
			Verb:     "impersonate",
			Group:    authenticationv1.GroupName,
			Version:  "v1",
			Resource: "uids",
			Name:     requested.uid,
		})
	}

	extraKeys := make([]string, 0, len(requested.extra))
	for key := range requested.extra {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		for _, value := range requested.extra[key] {
			attributes = append(attributes, authorizationv1.ResourceAttributes{
				Verb:        "impersonate",
				Group:       authenticationv1.GroupName,
				Version:     "v1",
				Resource:    "userextras",
				Subresource: key,
				Name:        value,
			})
		}
	}

	for _, attribute := range attributes {
		if err := s.authorizeImpersonationAttribute(ctx, requester, attribute); err != nil {
			return err
		}
	}
	return nil
}

func (s *serviceProxy) authorizeImpersonationAttribute(
	ctx context.Context,
	requester user.Info,
	attribute authorizationv1.ResourceAttributes,
) error {
	review, err := s.managedClusterKubeClient.AuthorizationV1().SubjectAccessReviews().Create(
		ctx,
		&authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               requester.GetName(),
				UID:                requester.GetUID(),
				Groups:             append([]string(nil), requester.GetGroups()...),
				Extra:              authorizationExtra(requester.GetExtra()),
				ResourceAttributes: &attribute,
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		internalErr := fmt.Errorf("failed to create SubjectAccessReview: %w", err)
		return newRequestProcessingError(http.StatusInternalServerError, "Internal Server Error", internalErr)
	}
	if review.Status.Allowed {
		return nil
	}
	if review.Status.EvaluationError != "" && !review.Status.Denied {
		internalErr := fmt.Errorf("SubjectAccessReview evaluation failed: %s", review.Status.EvaluationError)
		return newRequestProcessingError(http.StatusInternalServerError, "Internal Server Error", internalErr)
	}

	resource := attribute.Resource
	if attribute.Subresource != "" {
		resource += "/" + attribute.Subresource
	}
	message := fmt.Sprintf(
		"user %q is not allowed to impersonate %s %q",
		requester.GetName(),
		resource,
		attribute.Name,
	)
	if review.Status.Reason != "" {
		message += ": " + review.Status.Reason
	}
	return newRequestProcessingError(http.StatusForbidden, message, fmt.Errorf("%s", message))
}

func authorizationExtra(extra map[string][]string) map[string]authorizationv1.ExtraValue {
	if extra == nil {
		return nil
	}

	result := make(map[string]authorizationv1.ExtraValue, len(extra))
	for key, values := range extra {
		result[key] = append(authorizationv1.ExtraValue(nil), values...)
	}
	return result
}

func effectiveHubUser(hubUser user.Info) user.Info {
	username := hubUser.GetName()
	if strings.HasPrefix(username, serviceAccountUserPrefix) {
		username = "cluster:hub:" + username
	}

	return &user.DefaultInfo{
		Name:   username,
		UID:    hubUser.GetUID(),
		Groups: append([]string(nil), hubUser.GetGroups()...),
		Extra:  cloneUserExtra(hubUser.GetExtra()),
	}
}

func splitServiceAccountUsername(username string) (string, string, bool) {
	if !strings.HasPrefix(username, serviceAccountUserPrefix) {
		return "", "", false
	}

	parts := strings.Split(strings.TrimPrefix(username, serviceAccountUserPrefix), ":")
	if len(parts) != 2 ||
		len(utilvalidation.IsDNS1123Label(parts[0])) != 0 ||
		len(utilvalidation.IsDNS1123Subdomain(parts[1])) != 0 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func cloneUserExtra(extra map[string][]string) map[string][]string {
	if extra == nil {
		return nil
	}

	cloned := make(map[string][]string, len(extra))
	for key, values := range extra {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func impersonationForUser(info user.Info) *impersonationRequest {
	return &impersonationRequest{
		user:   info.GetName(),
		groups: append([]string(nil), info.GetGroups()...),
	}
}

func addOriginalUserAuditExtra(requested *impersonationRequest, original user.Info) error {
	if requested.extra == nil {
		requested.extra = map[string][]string{}
	}

	requested.extra[originalUserExtraUser] = []string{original.GetName()}
	if len(original.GetGroups()) > 0 {
		requested.extra[originalUserExtraGroups] = append([]string(nil), original.GetGroups()...)
	}
	if original.GetUID() != "" {
		requested.extra[originalUserExtraUID] = []string{original.GetUID()}
	}
	if len(original.GetExtra()) > 0 {
		encoded, err := json.Marshal(original.GetExtra())
		if err != nil {
			return fmt.Errorf("failed to encode original user extra: %w", err)
		}
		requested.extra[originalUserExtraExtra] = []string{string(encoded)}
	}
	return nil
}

// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker"
	"github.com/google/cel-go/common/ast"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	maxRules      = 32
	maxExpression = 4 << 10
	maxASTNodes   = 500
	maxRegexText  = 1 << 10
	maxCost       = 10_000
)

// ServiceAccount identifies the Kubernetes principal making a request.
type ServiceAccount struct {
	Namespace string
	Name      string
	UID       string
}

// Request describes an operation presented for authorization.
type Request struct {
	Operation string
	Branch    string
	Create    bool
	Delete    bool
	Force     bool
	Bucket    string
	Key       string
	Size      int64
}

// compiledRule pairs a named operation set with an executable CEL program.
type compiledRule struct {
	name       string
	operations map[string]struct{}
	program    cel.Program
}

// rule is the resource-neutral input to policy compilation.
type rule struct {
	name       string
	operations []string
	allow      string
}

// operationValidator reports whether an operation is valid for one resource kind.
type operationValidator func(string) bool

// Policy is an immutable compiled authorization policy.
type Policy struct {
	rules             []compiledRule
	revision          string
	validOperation    operationValidator
	accountDescriptor protoreflect.MessageDescriptor
	requestDescriptor protoreflect.MessageDescriptor
}

// costEstimator supplies conservative size estimates to CEL's cost model.
type costEstimator struct{}

// EstimateSize returns the maximum assumed collection size.
func (costEstimator) EstimateSize(checker.AstNode) *checker.SizeEstimate {
	size := checker.FixedSizeEstimate(1024)
	return &size
}

// EstimateCallCost delegates call costs to CEL's built-in estimator.
func (costEstimator) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// CompileGit validates and compiles Git authorization rules into a policy.
func CompileGit(apiRules []v1alpha1.GitAuthorizationRule) (*Policy, error) {
	rules := make([]rule, len(apiRules))
	for index, apiRule := range apiRules {
		rules[index] = rule{name: apiRule.Name, operations: apiRule.Operations, allow: apiRule.Allow}
	}
	return compile(rules, validGitOperation, []*descriptorpb.FieldDescriptorProto{
		messageField("operation", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		messageField("branch", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		messageField("create", 3, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
		messageField("delete", 4, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
		messageField("force", 5, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
	})
}

// CompileObject validates and compiles object-storage authorization rules into a policy.
func CompileObject(apiRules []v1alpha1.ObjectAuthorizationRule) (*Policy, error) {
	rules := make([]rule, len(apiRules))
	for index, apiRule := range apiRules {
		rules[index] = rule{name: apiRule.Name, operations: apiRule.Operations, allow: apiRule.Allow}
	}
	return compile(rules, validObjectOperation, []*descriptorpb.FieldDescriptorProto{
		messageField("operation", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		messageField("bucket", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		messageField("key", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		messageField("size", 4, descriptorpb.FieldDescriptorProto_TYPE_INT64),
	})
}

// compile validates normalized authorization rules for one resource kind.
func compile(rules []rule, valid operationValidator, requestFields []*descriptorpb.FieldDescriptorProto) (*Policy, error) {
	if len(rules) > maxRules {
		return nil, fmt.Errorf("policy has more than %d rules", maxRules)
	}
	environment, accountDescriptor, requestDescriptor, err := newEnvironment(requestFields)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	result := &Policy{
		rules: make([]compiledRule, 0, len(rules)), validOperation: valid,
		accountDescriptor: accountDescriptor, requestDescriptor: requestDescriptor,
	}
	names := make(map[string]struct{}, len(rules))
	// Validate resource bounds before constructing each executable rule.
	for _, rule := range rules {
		if _, exists := names[rule.name]; exists {
			return nil, fmt.Errorf("duplicate rule name %q", rule.name)
		}
		names[rule.name] = struct{}{}
		if len(rule.allow) > maxExpression {
			return nil, fmt.Errorf("rule %q expression exceeds 4 KiB", rule.name)
		}
		parsed, issues := environment.Compile(rule.allow)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("rule %q: %w", rule.name, issues.Err())
		}
		if parsed.OutputType() != cel.BoolType {
			return nil, fmt.Errorf("rule %q does not return bool", rule.name)
		}
		nodes, regexBytes := expressionComplexity(parsed.NativeRep().Expr())
		if nodes > maxASTNodes {
			return nil, fmt.Errorf("rule %q exceeds %d AST nodes", rule.name, maxASTNodes)
		}
		if regexBytes > maxRegexText {
			return nil, fmt.Errorf("rule %q exceeds 1 KiB regex text", rule.name)
		}
		estimate, err := environment.EstimateCost(parsed, costEstimator{})
		if err != nil || estimate.Max > maxCost {
			return nil, fmt.Errorf("rule %q exceeds estimated CEL cost", rule.name)
		}
		program, err := environment.Program(parsed, cel.CostLimit(maxCost))
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", rule.name, err)
		}
		operations := make(map[string]struct{}, len(rule.operations))
		for _, operation := range rule.operations {
			if !valid(operation) {
				return nil, fmt.Errorf("rule %q has unknown operation", rule.name)
			}
			operations[operation] = struct{}{}
		}
		result.rules = append(result.rules, compiledRule{name: rule.name, operations: operations, program: program})
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00", rule.name, strings.Join(rule.operations, ","), rule.allow)
	}
	result.revision = hex.EncodeToString(hash.Sum(nil))[:16]
	return result, nil
}

// expressionComplexity counts AST nodes and literal regular-expression bytes.
func expressionComplexity(expression ast.Expr) (int, int) {
	nodes, regexBytes := 0, 0
	ast.PostOrderVisit(expression, ast.NewExprVisitor(func(expr ast.Expr) {
		nodes++
		if call := expr.AsCall(); expr.Kind() == ast.CallKind && call.FunctionName() == "matches" {
			arguments := call.Args()
			if len(arguments) != 0 && arguments[len(arguments)-1].Kind() == ast.LiteralKind {
				if value, ok := arguments[len(arguments)-1].AsLiteral().Value().(string); ok {
					regexBytes += len(value)
				}
			}
		}
	}))
	return nodes, regexBytes
}

// validGitOperation reports whether operation belongs to the Git policy vocabulary.
func validGitOperation(operation string) bool {
	switch operation {
	case "git.read", "git.push":
		return true
	default:
		return false
	}
}

// validObjectOperation reports whether operation belongs to the object-storage policy vocabulary.
func validObjectOperation(operation string) bool {
	switch operation {
	case "bucket.list", "bucket.head", "object.list", "object.read", "object.write", "object.delete":
		return true
	default:
		return false
	}
}

// Revision returns a stable digest of the source rules.
func (p *Policy) Revision() string { return p.revision }

// Authorize evaluates applicable rules and returns the names that allowed the request.
func (p *Policy) Authorize(account ServiceAccount, request Request) (bool, []string, error) {
	if !p.validOperation(request.Operation) {
		return false, nil, errors.New("unknown operation")
	}
	accountValue := dynamicpb.NewMessage(p.accountDescriptor)
	setString(accountValue, "namespace", account.Namespace)
	setString(accountValue, "name", account.Name)
	setString(accountValue, "uid", account.UID)
	requestValue := dynamicpb.NewMessage(p.requestDescriptor)
	for index := range p.requestDescriptor.Fields().Len() {
		field := p.requestDescriptor.Fields().Get(index)
		switch field.Name() {
		case "operation":
			requestValue.Set(field, protoreflect.ValueOfString(request.Operation))
		case "branch":
			requestValue.Set(field, protoreflect.ValueOfString(request.Branch))
		case "create":
			requestValue.Set(field, protoreflect.ValueOfBool(request.Create))
		case "delete":
			requestValue.Set(field, protoreflect.ValueOfBool(request.Delete))
		case "force":
			requestValue.Set(field, protoreflect.ValueOfBool(request.Force))
		case "bucket":
			requestValue.Set(field, protoreflect.ValueOfString(request.Bucket))
		case "key":
			requestValue.Set(field, protoreflect.ValueOfString(request.Key))
		case "size":
			requestValue.Set(field, protoreflect.ValueOfInt64(request.Size))
		}
	}
	activation := map[string]any{
		"sa":      accountValue,
		"request": requestValue,
	}
	// A request is allowed when at least one applicable rule evaluates true.
	matched := make([]string, 0)
	for _, rule := range p.rules {
		if _, applies := rule.operations[request.Operation]; !applies {
			continue
		}
		value, _, err := rule.program.Eval(activation)
		if err != nil {
			return false, nil, err
		}
		allowed, ok := value.Value().(bool)
		if !ok {
			return false, nil, errors.New("CEL result was not bool")
		}
		if allowed {
			matched = append(matched, rule.name)
		}
	}
	return len(matched) != 0, matched, nil
}

// newEnvironment builds a CEL environment with one domain's request fields.
func newEnvironment(requestFields []*descriptorpb.FieldDescriptorProto) (*cel.Env, protoreflect.MessageDescriptor, protoreflect.MessageDescriptor, error) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String("sourcegateway/policy/types.proto"),
		Package: proto.String("sourcegateway.policy"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("ServiceAccount"), Field: []*descriptorpb.FieldDescriptorProto{
				messageField("namespace", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				messageField("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				messageField("uid", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			}},
			{Name: proto.String("Request"), Field: requestFields},
		},
	}, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	accountDescriptor := file.Messages().ByName("ServiceAccount")
	requestDescriptor := file.Messages().ByName("Request")
	environment, err := cel.NewEnv(
		cel.Types(dynamicpb.NewMessage(accountDescriptor), dynamicpb.NewMessage(requestDescriptor)),
		cel.Variable("sa", cel.ObjectType(string(accountDescriptor.FullName()))),
		cel.Variable("request", cel.ObjectType(string(requestDescriptor.FullName()))),
	)
	return environment, accountDescriptor, requestDescriptor, err
}

// messageField creates an optional protobuf field descriptor.
func messageField(name string, number int32, fieldType descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: fieldType.Enum()}
}

// setString assigns a string field on a dynamic message.
func setString(message *dynamicpb.Message, name protoreflect.Name, value string) {
	message.Set(message.Descriptor().Fields().ByName(name), protoreflect.ValueOfString(value))
}

package valid

import (
	"context"
	"reflect"
)

// RuleFunc is the custom function for data validation.
type RuleFunc func(ctx context.Context, in RuleFuncInput) error

// RuleFuncInput holds the input parameters that passed to custom rule function RuleFunc.
type RuleFuncInput struct {
	// Rule specifies the validation rule string, like "required", "between:1,100", etc.
	Rule string

	// Message specifies the custom error message or configured i18n message for this rule.
	Message string

	// Field specifies the field for this rule to validate.
	Field string

	// ValueType specifies the type of the value, which might be nil.
	ValueType reflect.Type

	// Value specifies the value for this rule to validate.
	Value *any

	// Data specifies the `data` which is passed to the Validator. It might be a type of map/struct or a nil value.
	// You can ignore the parameter `Data` if you do not really need it in your custom validation rule.
	Data *any
}

var (
	// customRuleFuncMap stores the custom rule functions.
	// map[Rule]RuleFunc
	customRuleFuncMap = make(map[string]RuleFunc)
)

// RegisterRule registers custom validation rule and function for package.
func RegisterRule(rule string, f RuleFunc) {
	if customRuleFuncMap[rule] != nil {
		return
	}
	customRuleFuncMap[rule] = f
}

// RegisterRuleByMap registers custom validation rules using map for package.
func RegisterRuleByMap(m map[string]RuleFunc) {
	for k, v := range m {
		customRuleFuncMap[k] = v
	}
}

// GetRegisteredRuleMap returns all the custom registered rules and associated functions.
func GetRegisteredRuleMap() map[string]RuleFunc {
	if len(customRuleFuncMap) == 0 {
		return nil
	}
	ruleMap := make(map[string]RuleFunc)
	for k, v := range customRuleFuncMap {
		ruleMap[k] = v
	}
	return ruleMap
}

// DeleteRule deletes custom defined validation one or more rules and associated functions from global package.
func DeleteRule(rules ...string) {
	for _, rule := range rules {
		delete(customRuleFuncMap, rule)
	}
}

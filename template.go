package agentflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A person step's question may hold ${{ }} expressions, filled in from the
// record when it is asked: the one place a value is put into text. Each is
// one expression, as a step's with takes it.
var (
	placeholder = regexp.MustCompile(`\$\{\{(.*?)\}\}`)
	plainRef    = regexp.MustCompile(`^[^{}()\s,]+$`)
	funcRef     = regexp.MustCompile(`^(latest|optional)\(([^{}()]*)\)$`)
)

// parseExpr reads the inside of one ${{ }}: a reference, or latest or
// optional of references.
func parseExpr(expr string) (Value, error) {
	expr = strings.TrimSpace(expr)
	if plainRef.MatchString(expr) {
		if _, err := parseRef(expr); err != nil {
			return Value{}, err
		}
		return From(expr), nil
	}
	m := funcRef.FindStringSubmatch(expr)
	if m == nil {
		return Value{}, fmt.Errorf("${{ %s }}: want a reference, latest(...) or optional(...)", expr)
	}
	var paths []string
	for _, p := range strings.Split(m[2], ",") {
		p = strings.TrimSpace(p)
		if !plainRef.MatchString(p) {
			return Value{}, fmt.Errorf("${{ %s }}: want references separated by commas", expr)
		}
		if _, err := parseRef(p); err != nil {
			return Value{}, err
		}
		paths = append(paths, p)
	}
	switch {
	case m[1] == "optional":
		return Optional(paths[0], paths[1:]...), nil
	case len(paths) == 1:
		return Value{}, errors.New("latest: one reference needs no latest")
	}
	return Latest(paths[0], paths[1:]...), nil
}

// templated says text holds an expression to fill in.
func templated(text string) bool { return strings.Contains(text, "${{") }

// template reads the expressions of a question's text, in order.
func template(text string) ([]Value, error) {
	var out []Value
	var errs []error
	for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
		v, err := parseExpr(m[1])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, v)
	}
	// What is left once every whole expression is taken out holds none.
	if rest := placeholder.ReplaceAllString(text, ""); templated(rest) {
		errs = append(errs, errors.New("a ${{ is never closed with }}"))
	}
	return out, errors.Join(errs...)
}

// questionValues are the values a person step's question is filled in
// with, none for any other step or a question that holds none.
func (n *Node) questionValues() []Value {
	if n.Kind != Person || !templated(n.Prompt) {
		return nil
	}
	values, _ := template(n.Prompt)
	return values
}

// checkTemplate holds a step's text to where expressions may be: only a
// person step's question takes them.
//
// A judge's prompt quotes the question it judges as the step writes it,
// and reads it filled in from its call: it is not a template of its own.
func (n *Node) checkTemplate() []error {
	if !templated(n.Prompt) || n.Verdict {
		return nil
	}
	if n.Kind != Person {
		return []error{fmt.Errorf("agentflow: node %q: only a person step's question takes ${{ }}; give a model step values through with", n.ID)}
	}
	if _, err := template(n.Prompt); err != nil {
		return []error{fmt.Errorf("agentflow: node %q: question: %w", n.ID, err)}
	}
	return nil
}

// rendered is the text of st's question, each expression filled in from
// the record: a string as it is, an optional with no value as nothing, and
// anything else as JSON.
func (st State) rendered(text string) (string, error) {
	if !templated(text) {
		return text, nil
	}
	var errs []error
	out := placeholder.ReplaceAllStringFunc(text, func(m string) string {
		v, err := parseExpr(placeholder.FindStringSubmatch(m)[1])
		if err != nil {
			errs = append(errs, err)
			return m
		}
		got, ok, err := st.resolve(v)
		switch {
		case err != nil:
			errs = append(errs, err)
			return m
		case !ok || got == nil:
			return ""
		}
		if s, isText := got.(string); isText {
			return s
		}
		b, err := json.Marshal(got)
		if err != nil {
			errs = append(errs, err)
			return m
		}
		return string(b)
	})
	return out, errors.Join(errs...)
}

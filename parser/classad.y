%{
package parser

import (
	"github.com/PelicanPlatform/classad/ast"
)

%}

%union {
	node      ast.Node
	expr      ast.Expr
	classad   *ast.ClassAd
	attr      *ast.AttributeAssignment
	attrs     []*ast.AttributeAssignment
	exprlist  []ast.Expr
	str       string
	integer   int64
	real      float64
	boolean   bool
}

%token <str> IDENTIFIER STRING_LITERAL
%token <integer> INTEGER_LITERAL
%token <real> REAL_LITERAL
%token <boolean> BOOLEAN_LITERAL
%token UNDEFINED ERROR
/* NEG is a '-' that libclassad lexes as part of a number literal: one where
   an operand is expected, directly before a digit or a '.' and a digit. The
   two make one operand ("-5"), a primary expression, so it can be the right
   operand of ?: and a postfix operator applies to it ("-5[0]" is (-5)[0]). The
   literal is represented as a unary minus applied to its magnitude. */
%token NEG
/* The magnitude 2^63 overflows a signed 64-bit int, so it is valid only after
   NEG (yielding INT64_MIN), and the lexer emits this token for it only there;
   anywhere else 2^63 is out of range and, as in libclassad, lexes as 0. */
%token INT64_MIN_MAGNITUDE
/* EXPR_START is never in the input: the lexer emits it first when it is asked
   to parse a standalone expression (ParseExpr), selecting the expression start
   rule instead of the ClassAd one. */
%token EXPR_START
/* ELVIS is the "?:" operator: a '?' followed by a ':' with only whitespace
   between them (since HTCONDOR-3814, libclassad 25.14, the lexer skips that
   whitespace; before it, a spaced "? :" was a low-precedence elvis). It is a
   high-precedence postfix operator alongside '.' and '[', matching the
   reference parser (10 ?: 2 + 3 is (10 ?: 2) + 3). */
%token ELVIS

/* Operators - ordered by precedence (lowest to highest) */
%left '?' ':'
%left OR
%left AND
%left '|'
%left '^'
%left '&'
%left EQ NE IS ISNT
%left '<' '>' LE GE
%left LSHIFT RSHIFT URSHIFT
%left '+' '-'
%left '*' '/' '%'
%right UNARY '!' '~'
/* ELVIS is just below '.' / '[' so the elvis right operand greedily grabs a
   trailing subscript/selection: (0) ?: {}[0] is (0) ?: ({}[0]), not
   ((0) ?: {})[0], matching the reference parser's recursive postfix right. */
%left ELVIS
%left '.' '[' '('

%type <classad> classad record_literal
%type <attrs> attr_list attr_seq
%type <attr> attr_assign
%type <str> strings
%type <expr> expr literal primary_expr postfix_expr unary_expr
%type <expr> mult_expr add_expr shift_expr rel_expr eq_expr
%type <expr> and_expr xor_expr or_expr logical_and_expr logical_or_expr
%type <expr> cond_expr
%type <exprlist> expr_list opt_expr_list arg_list opt_arg_list

%%

start
	: classad
		{
			if lex, ok := yylex.(interface{ SetResult(ast.Node) }); ok {
				lex.SetResult($1)
			}
		}
	| EXPR_START expr
		{
			if lex, ok := yylex.(interface{ SetResult(ast.Node) }); ok {
				lex.SetResult($2)
			}
		}
	;

classad
	: '[' attr_list ']'
		{ $$ = &ast.ClassAd{Attributes: $2} }
	;

record_literal
	: '[' attr_list ']'
		{ $$ = &ast.ClassAd{Attributes: $2} }
	;

/* Assignments are separated by ';', and extra ';' are allowed before, between
   and after them: "[;a=1;;b=2;]" and "[]" parse. As in libclassad's
   parseClassAd, a ';' with no assignment after it at all ("[;]", "[ ; ]") is
   an error, because a leading ';' must be followed by an assignment. Two
   assignments with no ';' between them ("[a=1 b=2]") is an error. */
attr_list
	: /* empty */
		{ $$ = []*ast.AttributeAssignment{} }
	| lead_semis attr_seq
		{ $$ = $2 }
	;

lead_semis
	: /* empty */
	| lead_semis ';'
	;

attr_seq
	: attr_assign
		{ $$ = []*ast.AttributeAssignment{$1} }
	| attr_seq ';'
		{ $$ = $1 }
	| attr_seq ';' attr_assign
		{ $$ = append($1, $3) }
	;

attr_assign
	: IDENTIFIER '=' expr
		{
			// libclassad's ClassAd::Insert refuses an empty name (''), which
			// fails the parse.
			if $1 == "" {
				yylex.Error("empty attribute name")
			}
			$$ = &ast.AttributeAssignment{Name: $1, Value: $3}
		}
	;

expr
	: cond_expr
		{ $$ = $1 }
	;

cond_expr
	: logical_or_expr
		{ $$ = $1 }
	| logical_or_expr '?' expr ':' cond_expr
		{ $$ = &ast.ConditionalExpr{Condition: $1, TrueExpr: $3, FalseExpr: $5} }
	;

logical_or_expr
	: logical_and_expr
		{ $$ = $1 }
	| logical_or_expr OR logical_and_expr
		{ $$ = &ast.BinaryOp{Op: "||", Left: $1, Right: $3} }
	;

logical_and_expr
	: or_expr
		{ $$ = $1 }
	| logical_and_expr AND or_expr
		{ $$ = &ast.BinaryOp{Op: "&&", Left: $1, Right: $3} }
	;

or_expr
	: xor_expr
		{ $$ = $1 }
	| or_expr '|' xor_expr
		{ $$ = &ast.BinaryOp{Op: "|", Left: $1, Right: $3} }
	;

xor_expr
	: and_expr
		{ $$ = $1 }
	| xor_expr '^' and_expr
		{ $$ = &ast.BinaryOp{Op: "^", Left: $1, Right: $3} }
	;

and_expr
	: eq_expr
		{ $$ = $1 }
	| and_expr '&' eq_expr
		{ $$ = &ast.BinaryOp{Op: "&", Left: $1, Right: $3} }
	;

eq_expr
	: rel_expr
		{ $$ = $1 }
	| eq_expr EQ rel_expr
		{ $$ = &ast.BinaryOp{Op: "==", Left: $1, Right: $3} }
	| eq_expr NE rel_expr
		{ $$ = &ast.BinaryOp{Op: "!=", Left: $1, Right: $3} }
	| eq_expr IS rel_expr
		{ $$ = &ast.BinaryOp{Op: "is", Left: $1, Right: $3} }
	| eq_expr ISNT rel_expr
		{ $$ = &ast.BinaryOp{Op: "isnt", Left: $1, Right: $3} }
	;

rel_expr
	: shift_expr
		{ $$ = $1 }
	| rel_expr '<' shift_expr
		{ $$ = &ast.BinaryOp{Op: "<", Left: $1, Right: $3} }
	| rel_expr '>' shift_expr
		{ $$ = &ast.BinaryOp{Op: ">", Left: $1, Right: $3} }
	| rel_expr LE shift_expr
		{ $$ = &ast.BinaryOp{Op: "<=", Left: $1, Right: $3} }
	| rel_expr GE shift_expr
		{ $$ = &ast.BinaryOp{Op: ">=", Left: $1, Right: $3} }
	;

shift_expr
	: add_expr
		{ $$ = $1 }
	| shift_expr LSHIFT add_expr
		{ $$ = &ast.BinaryOp{Op: "<<", Left: $1, Right: $3} }
	| shift_expr RSHIFT add_expr
		{ $$ = &ast.BinaryOp{Op: ">>", Left: $1, Right: $3} }
	| shift_expr URSHIFT add_expr
		{ $$ = &ast.BinaryOp{Op: ">>>", Left: $1, Right: $3} }
	;

add_expr
	: mult_expr
		{ $$ = $1 }
	| add_expr '+' mult_expr
		{ $$ = &ast.BinaryOp{Op: "+", Left: $1, Right: $3} }
	| add_expr '-' mult_expr
		{ $$ = &ast.BinaryOp{Op: "-", Left: $1, Right: $3} }
	;

mult_expr
	: unary_expr
		{ $$ = $1 }
	| mult_expr '*' unary_expr
		{ $$ = &ast.BinaryOp{Op: "*", Left: $1, Right: $3} }
	| mult_expr '/' unary_expr
		{ $$ = &ast.BinaryOp{Op: "/", Left: $1, Right: $3} }
	| mult_expr '%' unary_expr
		{ $$ = &ast.BinaryOp{Op: "%", Left: $1, Right: $3} }
	;

unary_expr
	: postfix_expr
		{ $$ = $1 }
	| '-' unary_expr %prec UNARY
		{ $$ = &ast.UnaryOp{Op: "-", Expr: $2} }
	| '+' unary_expr %prec UNARY
		{ $$ = &ast.UnaryOp{Op: "+", Expr: $2} }
	| '!' unary_expr
		{ $$ = &ast.UnaryOp{Op: "!", Expr: $2} }
	| '~' unary_expr
		{ $$ = &ast.UnaryOp{Op: "~", Expr: $2} }
	;

postfix_expr
	: primary_expr
		{ $$ = $1 }
	| postfix_expr '.' IDENTIFIER
		{ $$ = &ast.SelectExpr{Record: $1, Attr: $3} }
	| postfix_expr '[' expr ']'
		{ $$ = &ast.SubscriptExpr{Container: $1, Index: $3} }
	| postfix_expr ELVIS postfix_expr
		{ $$ = &ast.ElvisExpr{Left: $1, Right: $3} }
	| IDENTIFIER '(' opt_arg_list ')'
		{ $$ = &ast.FunctionCall{Name: $1, Args: $3} }
	;

primary_expr
	: literal
		{ $$ = $1 }
	| IDENTIFIER
		{
			name, scope := ParseScopedIdentifier($1)
			$$ = ast.NewAttributeReference(name, scope)
		}
	| '.' IDENTIFIER
		{
			// ".A" is an absolute reference: the reference engine resolves it
			// against the root ad (AttributeReference carries an `absolute`
			// flag, and attrrefs.cpp selects state.rootAd over state.curAd),
			// so it is NOT the same lookup as "A" inside a nested or chained
			// ad. An explicit inner scope (".MY.A") keeps that scope.
			name, scope := ParseScopedIdentifier($2)
			if scope == ast.NoScope {
				scope = ast.RootScope
			}
			$$ = ast.NewAttributeReference(name, scope)
		}
	| NEG INTEGER_LITERAL
		{ $$ = &ast.UnaryOp{Op: "-", Expr: &ast.IntegerLiteral{Value: $2}} }
	| NEG REAL_LITERAL
		{ $$ = &ast.UnaryOp{Op: "-", Expr: &ast.RealLiteral{Value: $2}} }
	| NEG INT64_MIN_MAGNITUDE
		{ $$ = &ast.IntegerLiteral{Value: -9223372036854775808} }
	| '(' expr ')'
		{ $$ = ast.Parenthesize($2) }
	| '{' opt_expr_list '}'
		{ $$ = &ast.ListLiteral{Elements: $2} }
	| record_literal
		{ $$ = &ast.RecordLiteral{ClassAd: $1} }
	;

literal
	: INTEGER_LITERAL
		{ $$ = &ast.IntegerLiteral{Value: $1} }
	| REAL_LITERAL
		{ $$ = &ast.RealLiteral{Value: $1} }
	| strings
		{ $$ = &ast.StringLiteral{Value: $1} }
	| BOOLEAN_LITERAL
		{ $$ = &ast.BooleanLiteral{Value: $1} }
	| UNDEFINED
		{ $$ = &ast.UndefinedLiteral{} }
	| ERROR
		{ $$ = &ast.ErrorLiteral{} }
	;

/* Adjacent string literals concatenate, C-style: "a" "b" is "ab". */
strings
	: STRING_LITERAL
		{ $$ = $1 }
	| strings STRING_LITERAL
		{ $$ = $1 + $2 }
	;

opt_expr_list
	: /* empty */
		{ $$ = []ast.Expr{} }
	| expr_list
		{ $$ = $1 }
	;

expr_list
	: expr
		{ $$ = []ast.Expr{$1} }
	| expr_list ',' expr
		{ $$ = append($1, $3) }
	;

/* Function-call arguments may be separated by ',' or ';' -- the reference
   engine treats the two interchangeably (f(a;b) == f(a,b)), evaluating both to
   the same value. (List literals, by contrast, accept only ','; "{1;2}" is a
   syntax error in the reference too, so they keep using expr_list.) */
opt_arg_list
	: /* empty */
		{ $$ = []ast.Expr{} }
	| arg_list
		{ $$ = $1 }
	;

arg_list
	: expr
		{ $$ = []ast.Expr{$1} }
	| arg_list ',' expr
		{ $$ = append($1, $3) }
	| arg_list ';' expr
		{ $$ = append($1, $3) }
	;

%%

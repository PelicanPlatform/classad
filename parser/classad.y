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
%left '.' '[' '('

%type <classad> classad record_literal
%type <attrs> attr_list
%type <attr> attr_assign
%type <expr> expr literal primary_expr postfix_expr unary_expr
%type <expr> mult_expr add_expr shift_expr rel_expr eq_expr
%type <expr> and_expr xor_expr or_expr logical_and_expr logical_or_expr
%type <expr> cond_expr
%type <exprlist> expr_list opt_expr_list

%%

start
	: classad
		{
			if lex, ok := yylex.(interface{ SetResult(ast.Node) }); ok {
				lex.SetResult($1)
			}
		}
	;

classad
	: '[' attr_list ']'
		{ $$ = &ast.ClassAd{Attributes: $2} }
	| '[' ']'
		{ $$ = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}} }
	;

record_literal
	: '[' attr_list ']'
		{ $$ = &ast.ClassAd{Attributes: $2} }
	| '[' ']'
		{ $$ = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}} }
	;

attr_list
	: attr_assign
		{ $$ = []*ast.AttributeAssignment{$1} }
	| attr_list ';' attr_assign
		{ $$ = append($1, $3) }
	| attr_list ';'
		{ $$ = $1 }
	;

attr_assign
	: IDENTIFIER '=' expr
		{ $$ = &ast.AttributeAssignment{Name: $1, Value: $3} }
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
	| logical_or_expr '?' ':' cond_expr
		{ $$ = &ast.ElvisExpr{Left: $1, Right: $4} }
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
	| IDENTIFIER '(' opt_expr_list ')'
		{ $$ = &ast.FunctionCall{Name: $1, Args: $3} }
	;

primary_expr
	: literal
		{ $$ = $1 }
	| IDENTIFIER
		{
			name, scope := ParseScopedIdentifier($1)
			$$ = &ast.AttributeReference{Name: name, Scope: scope}
		}
	| '(' expr ')'
		{ $$ = $2 }
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
	| STRING_LITERAL
		{ $$ = &ast.StringLiteral{Value: $1} }
	| BOOLEAN_LITERAL
		{ $$ = &ast.BooleanLiteral{Value: $1} }
	| UNDEFINED
		{ $$ = &ast.UndefinedLiteral{} }
	| ERROR
		{ $$ = &ast.ErrorLiteral{} }
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

%%

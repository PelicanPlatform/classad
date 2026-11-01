# Notes: findings in HTCondor's transform / config macro language

Observations recorded while reading HTCondor's C++ tree
(`/Users/bbockelm/projects/htcondor`, paths below are relative to it). Kept because the findings are
real and may be wanted later. No design commentary; nothing filed upstream; nothing in that tree
modified.

Everything here is derived by **reading** the source, not by building or running HTCondor. Where a
confirming test is cheap, it is named.

---

## 1. `if <general ClassAd expression>` returns the condition's truth as the validity flag

`Evaluate_config_if` (`src/condor_utils/config.cpp:707`) has the signature

```c
static bool Evaluate_config_if(const char * expr, bool & result, std::string & err_reason,
                               MACRO_SET & macro_set, MACRO_EVAL_CONTEXT & ctx)
```

where the **return value means "was this a valid conditional"** and the condition's truth is the
`bool & result` out-parameter. The branch that evaluates the text as a ClassAd expression —
reachable only when the context carries a bound ad — does neither:

```c
	// TODO: convert version & defined to booleans, and then evaluate the result as a ClassAd expression

	if ((ec == CIFT_COMPLEX) && ctx.is_context_ex && reinterpret_cast<MACRO_EVAL_CONTEXT_EX&>(ctx).ad) {
		classad::Value val;
		if (reinterpret_cast<MACRO_EVAL_CONTEXT_EX&>(ctx).ad->EvaluateExpr(expr, val)) {
			bool bval;
			if (val.IsBooleanValueEquiv(bval)) {
				return bval;
			}
		}
	}
	if (ec == CIFT_COMPLEX) {
		err_reason = "complex conditionals are not supported";
	} else {
		err_reason = "expression is not a conditional";
	}

	return false;
```
— `src/condor_utils/config.cpp:800-820`. `return bval` at `:805`-ish returns the condition's truth
as the validity flag and never writes `result`.

Tracing the one caller:

- `ConfigIfStack::line_is_if` initializes `bool bb = this->enabled();` — i.e. `true`, since it only
  evaluates when the enclosing block is enabled — and passes it as `result`
  (`src/condor_utils/config.cpp:913-914`).
- `Test_config_if_expression` copies that in-param: `bool value = result;`
  (`src/condor_utils/config.cpp:823-825`), calls `Evaluate_config_if(expr, value, ...)`
  (`:850`), and finishes `result = inverted ? !value : value; return valid;` (`:854-855`).

So:

| Condition evaluates to | Observed behavior |
|---|---|
| boolean-true | returns `true` (valid); `value` is never written and happens to still be `true` → branch taken. Correct by coincidence. |
| boolean-false | returns `false`, read by the caller as "invalid conditional" → `"<expr> is not a valid if condition"` (`src/condor_utils/config.cpp:915-916`) |
| `UNDEFINED` / `ERROR` / non-boolean | falls through to `err_reason = "complex conditionals are not supported"` (`:814-815`) |

An invalid `if` condition is not a warning. It aborts the parse:

```c
		if (ifstack.line_is_if(name, errmsg, macro_set, *pctx)) {
			if ( ! errmsg.empty()) {
				dprintf(D_CONFIG | D_ERROR_ALSO, "Parse_config if error: '%s' line: %s\n", errmsg.c_str(), name);
				config_errmsg = errmsg;
				retval = -1;
```
— `src/condor_utils/config.cpp:1546-1553`.

The context that reaches this branch is the ClassAd transform language, which is the only thing in
the tree that binds an ad into the macro evaluation context:

```c
	MACRO_EVAL_CONTEXT_EX & ctx = xfm.context();
	ctx.ad = input_ad;
	ctx.adname = "MY.";
```
— `src/condor_utils/xform_utils.cpp:1979-1981`, passed to `Parse_macros` at `:1997`.
`SubmitHash` uses the plain, non-`_EX` context (`src/condor_utils/submit_utils.h:805`, initialized
at `src/condor_utils/submit_utils.cpp:680`, which zeroes `is_context_ex` per
`src/condor_includes/condor_config.h:137-143`), so submit files never reach it.

The schedd converts a negative return into a submit rejection:

```c
rval = TransformClassAd(ad,*xfm,mset,errmsg,xfm_flags);
if (rval < 0) { ... return -3; }
```
— `src/condor_schedd.V6/job_transforms.cpp:170-180`. It passes the live job ad, not a copy, and
partial mutation is not rolled back (see §4).

**Status: confirmed by reading, twice, independently. Still wants a runtime test** — a
`condor_transform_ads` rules file containing `if JobUniverse == 99` plus a `SET`, against an ad
whose `JobUniverse` is 5, should print `"JobUniverse == 99 is not a valid if condition"` if this
reading is right.

The developers' own `TODO` immediately above the branch
(`src/condor_utils/config.cpp:800`) states the intended design:
*"convert version & defined to booleans, and then evaluate the result as a ClassAd expression."*

### Related: the `if` predicate grammar is a character-class classifier, not a parser

`Characterize_config_if_expression` (`src/condor_utils/config.cpp:578-678`) ORs character-class bits
over the text (`:596-624`) and matches whole bitmask values in a `switch` (`:628-671`); anything
unmatched becomes `CIFT_COMPLEX` (`:677`). The forms it admits are a number, `true`/`false`, the
"crufty bools" `yes`/`t`/`no`/`f` (`:692-705`), `version <op> <x.y[.z]>` (`:719-756`),
`defined <name|bool|number|use cat[:knob]>` (`:758-800`), a `$(VAR)` that expands to one of those
(`:830-836`), and a single leading `!` (`:838-842`). `if defined X && defined Y` is a syntax error.
Documented as *"Conditional if/else semantics are available in a limited form"*
(`docs/users-manual/submitting-a-job.rst:472`) and *"The `<simple condition>` does not permit
compound conditions"* (`:488`).

---

## 2. The transform language's loop is never driven by either daemon

`TRANSFORM [<n>] [<var>] in|from|matching (<items>)` reuses the submit `queue` iteration machinery
(`src/condor_utils/xform_utils.cpp:1092-1127`; loop-variable binding at `:1240-1270`; item advance
at `:1300-1321`). The parser stops reading at the `TRANSFORM` line:

```c
		const char * is_transform = is_xform_statement(line, "transform");
		if (is_transform) {
			// if this is an TRANSFORM statement, then we don't read any more statements.
```
— `src/condor_utils/xform_utils.cpp:1003-1006`; documented at
`docs/man-pages/condor_transform_ads.rst:25`.

The only call sites of the iteration API in the entire tree are in the standalone tool:

```
condor_tools/transform_ads.cpp:648:   will_iterate = args.xforms.back().first_iteration(args.mset);
condor_tools/transform_ads.cpp:665:   iterating = args.xforms.back().next_iteration(args.mset);
```
(`grep -rn "first_iteration\|next_iteration" --include=*.cpp src/` returns these two plus the two
definitions at `src/condor_utils/xform_utils.cpp:1274` and `:1300`.)

Neither the schedd's `job_transforms` nor the job router ever iterates. The docs do not say so.

Items are text, split on `", \t"` (`src/condor_utils/xform_utils.cpp:1253`); there is no list-value
iteration protocol, and `init_iterator` macro-expands the argument
(`src/condor_utils/xform_utils.cpp:1216`), so `TRANSFORM x in $(MY.SomeList)` would splice the
unparsed list expression text — braces and quotes included — and then tokenize that.

---

## 3. No resource bounds

| Bound | Value | Citation |
|---|---|---|
| step / fuel budget | **none** | no counter in `ParseRulesCallback`, `src/condor_utils/xform_utils.cpp:1768-1947` |
| `$()` expansion depth | **none** — `do { ... } while (special_id);` with no counter | `src/condor_utils/config.cpp:4805-4841` |
| iteration item count | **none** — uncapped `std::vector<std::string>` | `src/condor_utils/xform_utils.cpp:1101-1156` |
| statements per transform | **none** — merely counted into `step_count` | `src/condor_utils/xform_utils.cpp:1736`, `:2529` |
| `if` nesting | 63 (bit-stack in `unsigned long long`) | `src/condor_utils/config.cpp:860`; overflow error `:918` |
| `include` / `use` nesting | `CONFIG_MAX_NESTING_DEPTH` = 20 | `src/condor_includes/condor_config.h:43`; enforced `src/condor_utils/config.cpp:1125-1130`, `:1832-1835` |

`include` is disabled in transforms anyway (`src/condor_utils/xform_utils.cpp:292` sets
`CONFIG_OPT_NO_INCLUDE_FILE`; `docs/classads/transforms.rst:36`), but `use` is reachable.

Consequence: a cyclic macro definition (`A = $(B)`, `B = $(A)`) has nothing to stop it in
`expand_macro`.

---

## 4. Statement errors are logged and skipped; an abort leaves partial mutation

`ParseRulesCallback` returns `0 // line handled, keep scanning`
(`src/condor_utils/xform_utils.cpp:1946`) on every path except an unknown keyword, a missing
argument, or a malformed regex.

| Failure | Behavior | Citation |
|---|---|---|
| unknown keyword | abort (`-1`) | `src/condor_utils/xform_utils.cpp:1789-1793` |
| 2-arg keyword given 1 | abort, `"<KW> needs 2 arguments"` | `:1821-1823` |
| malformed regex (`/…`) | abort, `"invalid regex"` | `:1808-1811` |
| regex fails to compile | logged, statement skipped, "this entry will be ignored" | `:1931-1933` |
| `SET` RHS fails to parse | logged, attribute untouched, transform continues | `:1888-1894` |
| `EVALSET` RHS fails to parse or evaluate | logged, attribute untouched, transform continues | `:1905-1910` |
| `EVALMACRO` RHS fails | logged, macro not set, transform continues | `:1864-1865` |
| `if` condition invalid | abort (`retval = -1`) | `src/condor_utils/config.cpp:1546-1553` |

So a transform can complete "successfully" having applied a strict subset of its statements.

On abort, the statements already executed have already mutated the ad. The only rollback facility,
`XFormAdUndo` (`src/condor_utils/xform_utils.h:31-42`, `Revert` at
`src/condor_utils/xform_utils.cpp:1347`), is wired up as a dirty-attribute collector
(`src/condor_schedd.V6/schedd.cpp:15560`), never as error recovery.

An expression evaluating to `UNDEFINED` is not a failure: `EvaluateExpr` succeeds and
`XFormCopyValueToTree` falls through to `Literal::MakeLiteral`, so the ad gets `Foo = undefined`
stored literally with no warning (`src/condor_utils/xform_utils.cpp:1693-1716`). `EVALMACRO` of the
same expression stores the nine-character *string* `undefined`, because `XFormValueToString`
(`:1682-1691`) unparses it.

---

## 5. `ValidateXForm` never parses the right-hand side

```c
bool ValidateXForm (
	MacroStreamXFormSource & xfm, XFormHash & mset, int * step_count, std::string & errmsg)
{
	...
	_parse_rules_args args = { xfm, mset, nullptr, nullptr, nullptr, nullptr, 0, 0 };
	xfm.rewind();
	int rval = Parse_macros(xfm, 0, mset.macros(), READ_MACROS_SUBMIT_SYNTAX, &ctx, errmsg, ValidateRulesCallback, &args);
```
— `src/condor_utils/xform_utils.cpp:2513-2531`. `ValidateRulesCallback`
(`:1718-1761`) checks keyword spelling, argument count and regex compilability, then ends

```c
	return 0; // line is valid, keep scanning.
```
— `:1760`, without ever handing the RHS to the ClassAd parser.

So `SET Foo ((((` and `SET Foo $(TypoedName)` both pass config-load validation — which the job
router performs at startup (`src/condor_job_router/JobRouter.cpp:2969`) — and fail, or silently
no-op, only when a real ad flows through.

---

## 6. `$(MY.<attr>)` splices unparsed expression text with no parentheses

```c
	// lookup the macro in the given ad, but only if the name prefix matches
	if (ctx.is_context_ex) {
		MACRO_EVAL_CONTEXT_EX & ctxx = reinterpret_cast<MACRO_EVAL_CONTEXT_EX &>(ctx);
		if (ctxx.ad && starts_with_ignore_case(name, ctxx.adname)) {
			const char * attr = name + strlen(ctxx.adname);
			ExprTree * expr = ctxx.ad->Lookup(attr);
			if (expr) {
				if (ExprTreeIsLiteralString(expr, lval)) {
					// lval is already the string value
				} else {
					lval = ExprTreeToString(expr);
				}
			}
		}
	}
```
— `src/condor_utils/config.cpp:3205-3218`. Two consequences, both documented as warnings rather
than fixed: *"will not evaluate expressions"* (`docs/classads/transforms.rst:38-41`) and
*"Don't forget to quote string values!"* (`:51`).

### The documented SL6ToDocker example is affected

From `docs/admin-manual/ap-policy-configuration.rst:56-61`:

```
JOB_TRANSFORM_SL6ToDocker @=end
   # match only vanilla jobs that have WantSL6 and do not already have a DockerImage
   REQUIREMENTS JobUniverse==5 && WantSL6 && DockerImage =?= undefined
   SET  WantDocker = true
   SET  DockerImage = "SL6"
   SET  Requirements = TARGET.HasDocker && $(MY.Requirements)
@end
```

If the job's `Requirements` is `A || B`, the last line produces the text
`TARGET.HasDocker && A || B`. In the ClassAd grammar `&&` binds tighter than `||`
(`parser/classad.y:42-43` in `/Users/bbockelm/projects/golang-classads` — `%left OR` precedes
`%left AND`, so `OR` is the looser), so that parses as `(TARGET.HasDocker && A) || B`: whenever `B`
holds, the Docker requirement is discarded.

A literal string attribute expands **unquoted** by the same code, so `$(MY.Owner)` with
`Owner = "alice"` yields the bare token `alice` — an attribute reference, not a string.

---

## 7. `DEFAULT` doc/code divergence

The docs say `DEFAULT` applies "if that ClassAd does not currently have `<attr>` **or if it is
currently set to `undefined`**" (`docs/classads/transforms.rst:78-80`,
`docs/grid-computing/job-router.rst:445`). The implementation tests presence only:

```c
	case kw_DEFAULT:
		if (log_steps) log(*pargs, false, "DEFAULT %s to %s\n", attr.c_str(), rhs.ptr());
		if (ad.Lookup(attr) != NULL) {
			// this attribute already has a value.
			break;
		}
		// fall through
	case kw_SET:
```
— `src/condor_utils/xform_utils.cpp:1879-1885`. An attribute explicitly set to `undefined` is not
defaulted.

---

## 8. Minor notes

- The transform language is not a language with its own parser: it is a callback hung off the
  config-file parser. `TransformClassAd` calls `Parse_macros` with `ParseRulesCallback` as the
  "this line is not a `key=value` pair" handler (`src/condor_utils/xform_utils.cpp:1997`).
  `if`/`elif`/`else`/`endif`, `error :`, `warning :`, `@=tag` heredocs and `$()` expansion are all
  config-parser behavior.
- The keyword table is eleven entries: `COPY DEFAULT DELETE EVALMACRO EVALSET NAME RENAME
  REQUIREMENTS SET TRANSFORM UNIVERSE` (`src/condor_utils/xform_utils.cpp:1571-1583`, table at
  `:1587-1599`). No function definition form anywhere; the macro-function table is a fixed eleven
  entries plus the `$F[fpduwnxbqa]` family (`src/condor_utils/config.cpp:2694-2732`); ClassAd
  functions are C++-registered only (`src/classad/fnCall.cpp:294-312`).
- HTCondor's own converter from the legacy routing-table syntax emits temp-attribute scaffolding to
  work around having no typed locals, with these generated comments:
  `"# temporarily SET attrs because eval_set_ rules refer to them"`
  (`src/condor_utils/xform_utils.cpp:2458`) and `"# remove temporary attrs"` (`:2481`).
- One flat, global, mutable string namespace: the macro table is `name → const char *`
  (`src/condor_utils/xform_utils.h:240`), shared across a whole job-router route chain — the
  author's note at `src/condor_job_router/JobRouter.cpp:2893` observes that "the pre-route may leave
  behind some temp variables in the mset" — and route configuration knobs (`MaxJobs`,
  `GridResource`, `JobFailureTest`) live in that same namespace (`:2976-3003`), so a scratch
  variable named `MaxJobs` reconfigures the router.
- Submit files cannot see the job ad being built: the ClassAd branch of the conditional
  (`src/condor_utils/config.cpp:804`), the ad fallback in `lookup_macro` (`:3204-3217`) and `$EVAL`
  (`:3708-3732`, which constructs a fresh empty `ClassAd` for the non-`_EX` case) are all gated on
  `ctx.is_context_ex`. `queue` iterates job-ad materialization only: parsing stops at the first
  `queue` line (`src/condor_utils/submit_utils.cpp:9752-9771`) and the driver re-expands stored
  macros per item (`src/condor_submit.V6/submit.cpp:1507-1538`).
- `transform_on_eviction` smuggles a whole transform program into the job ad as `\x1e`-delimited
  text, unvalidated at submit time (`src/condor_utils/submit_utils.cpp:4290-4313`).

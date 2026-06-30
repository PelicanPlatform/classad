// C++ shim exposing the reference libclassad evaluation engine to Go via a
// plain C ABI (cgo cannot call C++ directly). It parses a ClassAd, evaluates
// every top-level attribute in the ad's scope, and serializes the results into
// the canonical encoding defined in fuzz/canon/canon.go. The Go-native engine
// produces byte-compatible output, so the differential fuzzer compares the two.
//
// Built automatically by cgo (it compiles .cc files in the package with $CXX
// and links -lclassad -lstdc++; see cgo.go for the directives).

#include "shim.h"

#include "classad/classad_distribution.h"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>
#include <algorithm>
#include <set>

using namespace classad;

namespace {

// Append a length-prefixed string field: "<len>,<bytes>".
void putLenStr(std::string &b, const std::string &s) {
	b += std::to_string(s.size());
	b += ',';
	b += s;
}

// Render a double round-trippably (%.17g) with canonical spellings for the
// non-finite values, matching canon.formatReal on the Go side.
void putReal(std::string &b, double r) {
	if (r != r) { b += "nan"; return; }            // NaN
	if (r > 0 && r * 0.5 == r) { b += "inf"; return; }   // +inf
	if (r < 0 && r * 0.5 == r) { b += "-inf"; return; }  // -inf
	char buf[64];
	snprintf(buf, sizeof(buf), "%.17g", r);
	b += buf;
}

// Forward declaration: encode an already-evaluated Value into out, using ad as
// the scope for lazily-evaluating any nested list elements / sub-ad attributes.
// `visiting` holds the list/classad pointers currently on the expansion path,
// to detect self-referential structures (e.g. A0 = {{A0}}): libclassad keeps
// such a list lazily circular, so expanding it would recurse forever -- report
// the cycle as error, matching the Go engine's cyclic-reference handling.
// encodeValue returns false when the value is a self-referential / cyclic
// structure (libclassad's element Evaluate fails partway through expanding a
// circular list); the caller then renders the whole containing value as a
// single error, so that A0 = {{A0}} encodes as error rather than a partially
// expanded list-of-lists -- matching the Go engine, which aborts a cyclic
// reference to error. A legitimate error *value* element (Evaluate succeeds,
// yielding error) is preserved, not collapsed.
bool encodeValue(std::string &out, const Value &v, const ClassAd *scope,
                 int depth, std::set<const void *> &visiting);

const int kMaxDepth = 64;

// Encode a ClassAd by evaluating each of its attributes (in its own scope),
// sorted by name, as a canonical 'C' value. A cyclic attribute value is
// contained as that attribute's error (the failure does not spread to siblings).
void encodeClassAd(std::string &out, const ClassAd *ad, int depth,
                   std::set<const void *> &visiting) {
	std::vector<std::string> names;
	for (auto it = ad->begin(); it != ad->end(); ++it) {
		names.push_back(it->first);
	}
	std::sort(names.begin(), names.end());

	out += 'C';
	out += std::to_string(names.size());
	out += ',';
	for (const auto &name : names) {
		Value v;
		if (!ad->EvaluateAttr(name, v)) {
			// Treat an attribute that fails to evaluate as error so the shape
			// still matches the Go side, which reports a value for every attr.
			v.SetErrorValue();
		}
		putLenStr(out, name);
		// Ignore the cyclic-failure return here: a cyclic attribute value is
		// rendered as that attribute's error and does not spread to siblings.
		encodeValue(out, v, ad, depth + 1, visiting);
	}
}

bool encodeValue(std::string &out, const Value &v, const ClassAd *scope,
                 int depth, std::set<const void *> &visiting) {
	if (depth > kMaxDepth) { out += 'E'; return true; }

	bool b;
	long long i;
	double r;
	const char *s = nullptr;
	const ExprList *lst = nullptr;
	const ClassAd *sub = nullptr;
	abstime_t at;

	if (v.IsUndefinedValue()) {
		out += 'U';
	} else if (v.IsErrorValue()) {
		out += 'E';
	} else if (v.IsBooleanValue(b)) {
		out += b ? "B1" : "B0";
	} else if (v.IsIntegerValue(i)) {
		out += 'I';
		out += std::to_string(i);
		out += ';';
	} else if (v.IsRealValue(r)) {
		out += 'R';
		putReal(out, r);
		out += ';';
	} else if (v.IsRelativeTimeValue(r)) {
		out += 'G';
		putReal(out, r);
		out += ';';
	} else if (v.IsAbsoluteTimeValue(at)) {
		out += 'A';
		putReal(out, (double)at.secs);
		out += ',';
		out += std::to_string(at.offset);
		out += ';';
	} else if (v.IsStringValue(s)) {
		out += 'S';
		putLenStr(out, std::string(s ? s : ""));
	} else if (v.IsListValue(lst)) {
		if (visiting.count(lst)) { out += 'E'; return false; }
		// Evaluate each element in the ad's scope. The static
		// ClassAd::EvaluateExpr cannot set the parent scope, so a bare
		// attribute reference inside an element (e.g. {a0}[0]) would wrongly
		// evaluate to undefined. Reconnect the list's parent scope to the ad
		// and evaluate each element with an EvalState rooted there, matching
		// how the subscript operator resolves list elements.
		ExprList *mlst = const_cast<ExprList *>(lst);
		if (scope != nullptr) {
			mlst->SetParentScope(scope);
		}
		std::vector<Value> evaled;
		for (auto it = mlst->begin(); it != mlst->end(); ++it) {
			Value ev;
			EvalState state;
			state.SetScopes(scope);
			if (*it == nullptr || !(*it)->Evaluate(state, ev)) {
				// A hard evaluation failure (e.g. libclassad's own guard firing
				// while expanding a self-referential list) collapses the whole
				// list to error -- distinct from an element that evaluates to a
				// legitimate error value, which is preserved below.
				out += 'E';
				return false;
			}
			evaled.push_back(ev);
		}
		// Encode elements into a local buffer so that a cyclic failure deeper in
		// renders this list as a single error rather than a partial structure.
		visiting.insert(lst);
		std::string body = "L";
		body += std::to_string(evaled.size());
		body += ',';
		bool ok = true;
		for (const auto &ev : evaled) {
			if (!encodeValue(body, ev, scope, depth + 1, visiting)) {
				ok = false;
				break;
			}
		}
		visiting.erase(lst);
		if (!ok) {
			out += 'E';
			return false;
		}
		out += body;
	} else if (v.IsClassAdValue(sub) && sub != nullptr) {
		if (visiting.count(sub)) { out += 'E'; return false; }
		visiting.insert(sub);
		encodeClassAd(out, sub, depth, visiting);
		visiting.erase(sub);
	} else {
		// Unknown / unhandled value kind.
		out += 'E';
	}
	return true;
}

} // namespace

extern "C" int classad_eval_ad(const char *adStr, char **out) {
	*out = nullptr;
	try {
		ClassAdParser parser;
		ClassAd ad;
		if (!parser.ParseClassAd(adStr, ad, true)) {
			return 0;
		}
		std::string encoded;
		std::set<const void *> visiting;
		encodeClassAd(encoded, &ad, 0, visiting);
		*out = strdup(encoded.c_str());
		return 1;
	} catch (...) {
		if (*out) { free(*out); *out = nullptr; }
		return 0;
	}
}

extern "C" void classad_free(char *p) {
	free(p);
}

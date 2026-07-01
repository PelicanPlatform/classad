//go:build libclassad

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

using namespace classad;

// ClassAdReconfig (from libcondor_utils) registers HTCondor's extra ClassAd
// functions -- split, splitUserName, splitSlotName, and the stringList* family
// -- which bare libclassad does not include. The classad2 Python bindings and
// the classad_eval CLI call it on startup; the shim must too, or those
// functions evaluate to error here and manufacture false divergences against
// the Go engine (which implements them). It is a plain C++ symbol (not extern
// "C") exported by libcondor_utils.
void ClassAdReconfig();

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
void encodeValue(std::string &out, const Value &v, const ClassAd *scope, int depth);

// kMaxDepth bounds expansion of recursive / self-referential structures
// (e.g. A0 = {{A0}}, which libclassad keeps as a lazily circular list). The Go
// canonical encoder (canon.FromGoValue) uses the same limit, so both sides
// produce the same depth-truncated form for a circular value.
const int kMaxDepth = 64;

// Encode a ClassAd by evaluating each of its attributes (in its own scope),
// sorted by name, as a canonical 'C' value.
void encodeClassAd(std::string &out, const ClassAd *ad, int depth) {
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
		encodeValue(out, v, ad, depth + 1);
	}
}

void encodeValue(std::string &out, const Value &v, const ClassAd *scope, int depth) {
	if (depth > kMaxDepth) { out += 'E'; return; }

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
				ev.SetErrorValue();
			}
			evaled.push_back(ev);
		}
		out += 'L';
		out += std::to_string(evaled.size());
		out += ',';
		for (const auto &ev : evaled) {
			encodeValue(out, ev, scope, depth + 1);
		}
	} else if (v.IsClassAdValue(sub) && sub != nullptr) {
		encodeClassAd(out, sub, depth);
	} else {
		// Unknown / unhandled value kind.
		out += 'E';
	}
}

} // namespace

extern "C" int classad_eval_ad(const char *adStr, char **out) {
	// Register HTCondor's extra ClassAd functions once, before the first
	// evaluation (thread-safe function-local static initialization).
	static const bool reconfigured = [] { ClassAdReconfig(); return true; }();
	(void)reconfigured;

	*out = nullptr;
	try {
		ClassAdParser parser;
		ClassAd ad;
		if (!parser.ParseClassAd(adStr, ad, true)) {
			return 0;
		}
		std::string encoded;
		encodeClassAd(encoded, &ad, 0);
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

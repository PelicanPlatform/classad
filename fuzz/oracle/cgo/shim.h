//go:build libclassad

#ifndef CLASSAD_FUZZ_SHIM_H
#define CLASSAD_FUZZ_SHIM_H

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Parse adStr as a single ClassAd and evaluate every top-level attribute in
 * the ad's own scope, producing a canonical encoding (see fuzz/canon) of the
 * resulting classad value.
 *
 * Return value:
 *    1  parse succeeded; *out points to a malloc'd canonical encoding string
 *       (a 'C...' classad value). Caller must classad_free() it.
 *    0  parse failed; *out is left NULL.
 *
 * The implementation catches all C++ exceptions and converts them to a parse
 * failure or an 'E' (error) value, so a malformed expression cannot unwind
 * across the cgo boundary. A hard crash (segfault/abort) inside libclassad is
 * still possible and is itself a finding; drivers journal the input first.
 */
int classad_eval_ad(const char *adStr, char **out);

/*
 * Like classad_eval_ad, but parses adStr as an OLD-ClassAd (the newline-separated,
 * unbracketed wire format daemons advertise), via ClassAdParser::SetOldClassAd(true).
 * Old-ClassAd string literals get no escape processing (Lexer::tokenizeStringOld) -- the
 * behavior the Go engine's ParseOld must match. Same return convention as classad_eval_ad.
 */
int classad_eval_ad_old(const char *adStr, char **out);

/* Free a string previously returned via classad_eval_ad's out parameter. */
void classad_free(char *p);

#ifdef __cplusplus
}
#endif

#endif

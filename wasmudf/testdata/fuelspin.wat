;; fuelspin -- an instrumented runaway: it never terminates on its own, but it
;; declares a step cost on every iteration, so the host's step budget stops it at
;; exactly the same iteration on every machine. This is the fixture the step limit
;; is for; hardspin.wat is the one it cannot stop.
(module
  (import "classad_udf" "fuel" (func $fuel (param i32)))
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    (loop $spin
      (call $fuel (i32.const 1))
      (br $spin))
    (i32.const 0))
)

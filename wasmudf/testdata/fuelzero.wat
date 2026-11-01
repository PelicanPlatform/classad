;; fuelzero -- a runaway that declares a cost of ZERO on every iteration. A budget
;; that took the declared cost at face value would never charge it anything, and a
;; guest could opt out of being metered simply by charging nothing. Calling the
;; import is itself a step, so this is bounded exactly like fuelspin.
(module
  (import "classad_udf" "fuel" (func $fuel (param i32)))
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    (loop $spin
      (call $fuel (i32.const 0))
      (br $spin))
    (i32.const 0))
)

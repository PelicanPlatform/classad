;; trap -- a guest that always traps. A trap is the guest's whole failure surface
;; (unreachable, an out-of-bounds access, an integer divide by zero and a stack
;; overflow all arrive at the host as one), so one fixture covers the class.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    unreachable)
)

;; status -- always reports a nonzero status without writing the result slot. A
;; guest that cannot produce a value says so this way instead of having to encode
;; ERROR, and the host must not read the slot it was told not to trust.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    (i32.const 42))
)

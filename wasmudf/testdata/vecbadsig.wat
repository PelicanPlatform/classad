;; vecbadsig -- the vector entry point with the wrong arity. A load error, for the
;; same reason the scalar signature is checked at load: a mismatch found at call
;; time would be one declined batch per block with nothing naming the cause.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call_vec") (param i32 i32 i32) (result i32) (i32.const 0))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32) (i32.const 0))
)

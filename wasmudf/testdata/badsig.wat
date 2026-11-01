;; badsig -- the right export name with the wrong signature. Rejected at load,
;; because a signature mismatch found at call time would be one ERROR per record
;; with nothing naming the cause.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32) (result i32) (local.get 0))
)

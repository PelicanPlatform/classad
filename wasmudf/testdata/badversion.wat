;; badversion -- a module built against an ABI this host does not speak. The
;; version export exists so that mismatch is a load error naming both numbers,
;; rather than a guest reading slots at offsets the host never wrote.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 7))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32) (i32.const 0))
)

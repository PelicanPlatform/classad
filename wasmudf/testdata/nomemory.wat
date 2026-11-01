;; nomemory -- no exported memory, so there is nowhere to pass a value. Load error.
(module
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32) (i32.const 0))
)

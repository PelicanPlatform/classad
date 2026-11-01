;; bigmem -- declares more memory than the loader's cap allows. Refused at load,
;; by the runtime, before any of it is allocated.
(module
  (memory (export "memory") 200)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32) (i32.const 0))
)

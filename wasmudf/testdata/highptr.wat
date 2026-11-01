;; highptr -- returns a string whose pointer has bits set above 32. wasm32 offsets
;; are 32 bits, so the upper word must be zero; truncating it instead would read a
;; different, in-bounds region and return a plausible wrong string.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 5))
    (i32.store offset=4 (local.get $res) (i32.const 1))
    (i64.store offset=8 (local.get $res) (i64.const 0x100000000))
    (i32.const 0))
)

;; badbool -- a boolean slot whose payload is neither 0 nor 1. The tag and the
;; payload have to agree or the slot does not mean anything.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 2))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 5))
    (i32.const 0))
)

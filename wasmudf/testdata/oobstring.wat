;; oobstring -- returns a string slot pointing past the end of linear memory. The
;; guest cannot reach outside its memory itself, but it CAN hand the host a pointer
;; that does, so the host validates every guest-chosen pointer it reads.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 5))
    (i32.store offset=4 (local.get $res) (i32.const 32))
    (i64.store offset=8 (local.get $res) (i64.const 0xfffff0))
    (i32.const 0))
)

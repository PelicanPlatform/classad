;; vectrap -- the vector entry point traps; the scalar one works.
;;
;; The whole batch must become ERROR, and NOTHING may be escalated: the decline
;; mask a dead guest left behind says nothing about which records it handled, so
;; retrying them on the scalar path would mean the same input giving a scalar
;; answer or an ERROR depending on where the guest happened to die.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call_vec") (param i32 i32 i32 i32 i32) (result i32)
    unreachable)
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 42))
    (i32.const 0))
)

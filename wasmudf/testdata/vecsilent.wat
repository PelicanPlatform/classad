;; vecsilent -- reports success, declines nothing, writes nothing.
;;
;; Every element still holds the poison the host wrote, so this is the batch-wide
;; version of the stale-slot problem: without the poison the column would be
;; whatever the previous batch computed, for a success status, with no failure
;; counted anywhere.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call_vec") (param i32 i32 i32 i32 i32) (result i32)
    (i32.const 0))
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 1))
    (i32.const 0))
)

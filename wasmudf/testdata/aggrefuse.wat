;; aggrefuse -- produces an identity, then returns a negative length from fold.
;;
;; A negative length is a guest DECLINING, which is a different thing from a guest
;; writing past its buffer, and the host keeps the two apart. There is nowhere for
;; a declined fold to go: a scalar function that declines a record has a per-record
;; path to fall back to, and an aggregate has no such thing, so the query fails.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param i32) (param i32) (param i32) (param i32)
              (param i32) (param i32) (result i32)
    (i32.const -1))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
    (i64.store (local.get $op) (i64.load (local.get $ap)))
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.store (local.get $rp) (i32.const 3))
    (i32.store offset=4 (local.get $rp) (i32.const 0))
    (i64.store offset=8 (local.get $rp) (i64.load (local.get $sp)))
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)

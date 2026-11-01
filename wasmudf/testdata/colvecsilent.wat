;; colvecsilent -- returns success from a columnar call without writing the one
;; state byte that describes its output column.
;;
;; Version 1 poisons n result state bytes and catches this at the first element.
;; The columnar result has one, so this fixture is what proves that one is still
;; enough to catch a guest that produced nothing: the poison survives, the host
;; refuses the batch, and the records get ERROR rather than whatever the previous
;; batch left in the column.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32)
    ;; Payloads, diligently. State byte, never.
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
      (i64.store (i32.add (i32.load offset=12 (local.get $res))
                          (i32.shl (local.get $i) (i32.const 3)))
                 (i64.const 7))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)

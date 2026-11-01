;; colvecbadbool -- a boolean result column with a payload that is not 0 or 1.
;;
;; The engine reads any nonzero payload as TRUE and the scalar path calls it
;; malformed, so a guest allowed to write 2 here could get different answers from
;; its own two entry points for the same input. The columnar form checks this with
;; one OR per lane rather than a branch per lane, and this is what the check is
;; for: every lane is 1 except the last, which is 2.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $rI i32)
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 2))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
      (i64.store (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3)))
                 (i64.const 1))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
    (i64.store (i32.add (local.get $rI)
                        (i32.shl (i32.sub (local.get $n) (i32.const 1)) (i32.const 3)))
               (i64.const 2))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)

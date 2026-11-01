;; vecstatus -- writes a full result column, sets every decline bit, and then
;; reports a nonzero status.
;;
;; It is the fixture for one rule: the decline mask is read only when the call
;; reported success. A host that read the mask first would escalate every record
;; of a call that told it the results were not valid.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $w i32) (local $rSt i32) (local $rI i32)
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 0))
        (i64.store (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3)))
                   (i64.const 5))
        (local.set $w (i32.add (local.get $decl)
                               (i32.shl (i32.shr_u (local.get $i) (i32.const 6)) (i32.const 3))))
        (i64.store (local.get $w)
          (i64.or (i64.load (local.get $w))
                  (i64.shl (i64.const 1)
                           (i64.extend_i32_u (i32.and (local.get $i) (i32.const 63))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 9))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 77))
    (i32.const 0))
)

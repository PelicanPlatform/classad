;; vecconstdecline -- vecconst with an escape hatch: add(k, x) where k is a
;; literal, declining the records whose x is odd.
;;
;; It covers one path nothing else does: a CONST argument surviving an
;; escalation. The host boxes a declined record's arguments from the caller's
;; columns, and a const column has exactly one element however many records it
;; stands for -- so a host that indexed it by record number would read off the end
;; of a one-element slice the moment any record escalated.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func $setdecl (param $decl i32) (param $i i32)
    (local $w i32)
    (local.set $w (i32.add (local.get $decl)
                           (i32.shl (i32.shr_u (local.get $i) (i32.const 6)) (i32.const 3))))
    (i64.store (local.get $w)
      (i64.or (i64.load (local.get $w))
              (i64.shl (i64.const 1)
                       (i64.extend_i32_u (i32.and (local.get $i) (i32.const 63)))))))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $k i64) (local $iPtr i32)
    (local $rSt i32) (local $rI i32)
    (local $i i32) (local $off i32) (local $v i64)

    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    ;; Argument 0 must arrive const, with only element 0 written.
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 1))
      (then (return (i32.const 2))))
    (local.set $k (i64.load (i32.load offset=12 (local.get $args))))
    (local.set $iPtr (i32.load offset=28 (local.get $args)))
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $off (i32.shl (local.get $i) (i32.const 3)))
        (local.set $v (i64.load (i32.add (local.get $iPtr) (local.get $off))))
        (if (i64.ne (i64.and (local.get $v) (i64.const 1)) (i64.const 0))
          (then (call $setdecl (local.get $decl) (local.get $i)))
          (else
            (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 0))
            (i64.store (i32.add (local.get $rI) (local.get $off))
                       (i64.add (local.get $k) (local.get $v)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.add (i64.load offset=8 (local.get $a))
               (i64.load offset=24 (local.get $a))))
    (i32.const 0))
)

;; colvecconst -- multiplies a column by a const argument, and REQUIRES the const
;; flag on it.
;;
;; A literal argument is the commonest shape a UDF call in a query has, and it
;; crosses as one state byte, one payload and a flag whatever n is. Refusing the
;; batch when the flag is missing is what stops the host from quietly materializing
;; it and the saving from disappearing without anything noticing.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $k i64) (local $iPtr i32) (local $rI i32) (local $off i32)
    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    ;; argument 0: columnar, const, integer
    (if (i32.ne (i32.load (local.get $args)) (i32.const 2))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 1))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load8_u (i32.load offset=8 (local.get $args))) (i32.const 0))
      (then (return (i32.const 1))))
    ;; argument 1: columnar, not const, integer
    (if (i32.ne (i32.load offset=16 (local.get $args)) (i32.const 2))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=20 (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load8_u (i32.load offset=24 (local.get $args))) (i32.const 0))
      (then (return (i32.const 1))))

    (local.set $k (i64.load (i32.load offset=12 (local.get $args))))
    (local.set $iPtr (i32.load offset=28 (local.get $args)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
      (local.set $off (i32.shl (local.get $i) (i32.const 3)))
      (i64.store (i32.add (local.get $rI) (local.get $off))
        (i64.mul (i64.load (i32.add (local.get $iPtr) (local.get $off))) (local.get $k)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a))
               (i64.load offset=24 (local.get $a))))
    (i32.const 0))
)

;; vecmaskarg -- a vectorized UDF whose argument is in the MASK form.
;;
;; isTrue(x): the argument is a column of three-valued logic held as two
;; bitplanes, two bits per record, which is what a comparison in the engine
;; already produces. The host passes the planes through untouched, so a batch of
;; 4096 records arrives as 1KB rather than the 36KB the data form would need --
;; and that is the representation the engine HAS, not one anybody converted to.
;;
;; A descriptor in the mask form puts the hi plane at aPtr and the lo plane at
;; bPtr. (hi,lo) = 00 FALSE, 01 TRUE, 10 UNDEFINED, 11 ERROR.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  ;; bitof returns bit $i of the plane at $plane, as 0 or 1.
  (func $bitof (param $plane i32) (param $i i32) (result i32)
    (i32.wrap_i64
      (i64.and
        (i64.shr_u
          (i64.load (i32.add (local.get $plane)
                             (i32.shl (i32.shr_u (local.get $i) (i32.const 6)) (i32.const 3))))
          (i64.extend_i32_u (i32.and (local.get $i) (i32.const 63))))
        (i64.const 1))))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $hi i32) (local $lo i32)
    (local $rSt i32) (local $rI i32)
    (local $i i32) (local $isTrue i32)

    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    ;; Mask form only: this module is about the bitplanes, so a data-form
    ;; argument is the host not having passed what it said it would.
    (if (i32.ne (i32.load (local.get $args)) (i32.const 1))
      (then (return (i32.const 1))))

    (local.set $hi (i32.load offset=8 (local.get $args)))
    (local.set $lo (i32.load offset=12 (local.get $args)))
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        ;; TRUE is hi=0, lo=1 and nothing else.
        (local.set $isTrue
          (i32.and (i32.eqz (call $bitof (local.get $hi) (local.get $i)))
                   (call $bitof (local.get $lo) (local.get $i))))
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 2)) ;; bool
        (i64.store (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3)))
                   (i64.extend_i32_u (local.get $isTrue)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  ;; The scalar fallback, for a record the vector path declines (it declines
  ;; none) and for a caller that uses this UDF per record.
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 2))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.extend_i32_u
        (i32.and (i32.eq (i32.load (local.get $a)) (i32.const 2))
                 (i64.ne (i64.load offset=8 (local.get $a)) (i64.const 0)))))
    (i32.const 0))
)

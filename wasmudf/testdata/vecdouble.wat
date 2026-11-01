;; vecdouble -- the canonical vectorized UDF: doubles a number.
;;
;; Its vector entry point implements ONE typed path, integers, over the whole
;; batch, and declines every other element state through the escape hatch. That is
;; the shape the escape hatch exists for: the guest does not have to express
;; UNDEFINED, ERROR, reals and mixed types in branch-free code to be allowed to
;; vectorize the case its data is actually made of.
;;
;; Its scalar entry point is the complete implementation -- integers, reals,
;; UNDEFINED and ERROR -- and it is what the declined records are served by.
;; The two agree on every input both accept, which is what makes the escape hatch
;; invisible in the answers and visible only in the counters.
;;
;; A batch descriptor is 16 bytes: form u32 @0, flags u32 @4, aPtr u32 @8,
;; bPtr u32 @12. In the data form aPtr is n state bytes and bPtr is n i64s.
;; States: 0 int, 1 real, 2 bool, 3 undefined, 4 error.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  ;; setdecl sets bit $i of the decline mask at $decl -- "I did not handle this
  ;; record". The host clears the mask before every call, so a set bit always
  ;; means this call.
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
    (local $stPtr i32) (local $iPtr i32)
    (local $rSt i32) (local $rI i32)
    (local $i i32) (local $off i32)

    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    ;; Data form, and not const: an all-const batch never reaches here (the host
    ;; computes it once through the scalar path), so a const flag would mean the
    ;; host and this module disagree about the ABI.
    (if (i32.ne (i32.load (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))

    (local.set $stPtr (i32.load offset=8 (local.get $args)))
    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $off (i32.shl (local.get $i) (i32.const 3)))
        (if (i32.eq (i32.load8_u (i32.add (local.get $stPtr) (local.get $i))) (i32.const 0))
          (then
            (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 0))
            (i64.store (i32.add (local.get $rI) (local.get $off))
              (i64.mul (i64.load (i32.add (local.get $iPtr) (local.get $off)))
                       (i64.const 2))))
          (else
            ;; Not an integer. Hand it back rather than guess, and leave the
            ;; result slot alone -- the host never reads a declined slot.
            (call $setdecl (local.get $decl) (local.get $i))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  ;; The scalar entry point: the complete function, and the escape hatch's target.
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (local $tag i32)
    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (local.set $tag (i32.load (local.get $a)))
    (i32.store (local.get $res) (local.get $tag))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 0))
    (block $done
      (if (i32.eq (local.get $tag) (i32.const 3)) ;; integer
        (then
          (i64.store offset=8 (local.get $res)
            (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 4)) ;; real
        (then
          (i64.store offset=8 (local.get $res)
            (i64.reinterpret_f64
              (f64.mul (f64.reinterpret_i64 (i64.load offset=8 (local.get $a)))
                       (f64.const 2))))
          (br $done)))
      ;; undefined and error pass through with the zero payload already written
      (if (i32.le_u (local.get $tag) (i32.const 1))
        (then (br $done)))
      (return (i32.const 2)))
    (i32.const 0))
)

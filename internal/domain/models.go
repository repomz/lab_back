package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Role string

const (
	RolePatient Role = "patient"
	RoleDoctor  Role = "doctor"
)

type User struct {
	ID               primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Email            string             `bson:"email" json:"email"`
	PasswordHash     string             `bson:"password_hash" json:"-"`
	Role             Role               `bson:"role" json:"role"`
	FullName         string             `bson:"full_name" json:"full_name"`
	Specialization   string             `bson:"specialization,omitempty" json:"specialization,omitempty"`
	LicenseNumber    string             `bson:"license_number,omitempty" json:"license_number,omitempty"`
	Verified         bool               `bson:"verified" json:"verified"`
	PatientProfile   *PatientProfile    `bson:"patient_profile,omitempty" json:"patient_profile,omitempty"`
	HomeVisits       bool               `bson:"home_visits,omitempty" json:"home_visits,omitempty"`
	AppointmentSlots []time.Time        `bson:"appointment_slots,omitempty" json:"appointment_slots,omitempty"`
	AvatarPath       string             `bson:"avatar_path,omitempty" json:"-"`
	AvatarPreset     string             `bson:"avatar_preset,omitempty" json:"avatar_preset,omitempty"`
	AvatarUpdatedAt  *time.Time         `bson:"avatar_updated_at,omitempty" json:"avatar_updated_at,omitempty"`
	CreatedAt        time.Time          `bson:"created_at" json:"created_at"`
}

type ActivitySurvey struct {
	RegularSport  bool   `bson:"regular_sport" json:"regular_sport"`
	SportType     string `bson:"sport_type,omitempty" json:"sport_type,omitempty"`
	Employment    string `bson:"employment,omitempty" json:"employment,omitempty"`
	WorkActivity  string `bson:"work_activity,omitempty" json:"work_activity,omitempty"`
	WeeklyMinutes int    `bson:"weekly_minutes,omitempty" json:"weekly_minutes,omitempty"`
}

type NutritionSurvey struct {
	FattyFood      string `bson:"fatty_food,omitempty" json:"fatty_food,omitempty"`
	FastCarbs      string `bson:"fast_carbs,omitempty" json:"fast_carbs,omitempty"`
	Vegetables     string `bson:"vegetables,omitempty" json:"vegetables,omitempty"`
	MealRegularity string `bson:"meal_regularity,omitempty" json:"meal_regularity,omitempty"`
}

type PatientProfile struct {
	Age                     int             `bson:"age" json:"age"`
	HeightCM                float64         `bson:"height_cm" json:"height_cm"`
	WeightKG                float64         `bson:"weight_kg" json:"weight_kg"`
	BMI                     float64         `bson:"bmi" json:"bmi"`
	Activity                ActivitySurvey  `bson:"activity" json:"activity"`
	Nutrition               NutritionSurvey `bson:"nutrition" json:"nutrition"`
	ActivityRecommendation  string          `bson:"activity_recommendation,omitempty" json:"activity_recommendation,omitempty"`
	NutritionRecommendation string          `bson:"nutrition_recommendation,omitempty" json:"nutrition_recommendation,omitempty"`
	UpdatedAt               time.Time       `bson:"updated_at" json:"updated_at"`
}

type MarkerStatus string

const (
	StatusLow     MarkerStatus = "low"
	StatusNormal  MarkerStatus = "normal"
	StatusHigh    MarkerStatus = "high"
	StatusUnknown MarkerStatus = "unknown"
)

type Marker struct {
	Name          string       `bson:"name" json:"name"`
	CanonicalName string       `bson:"canonical_name" json:"canonical_name"`
	Value         *float64     `bson:"value,omitempty" json:"value,omitempty"`
	TextValue     string       `bson:"text_value,omitempty" json:"text_value,omitempty"`
	Unit          string       `bson:"unit,omitempty" json:"unit,omitempty"`
	ReferenceMin  *float64     `bson:"reference_min,omitempty" json:"reference_min,omitempty"`
	ReferenceMax  *float64     `bson:"reference_max,omitempty" json:"reference_max,omitempty"`
	ReferenceText string       `bson:"reference_text,omitempty" json:"reference_text,omitempty"`
	Status        MarkerStatus `bson:"status" json:"status"`
	Confidence    float64      `bson:"confidence,omitempty" json:"confidence,omitempty"`
	Warnings      []string     `bson:"warnings,omitempty" json:"warnings,omitempty"`
}

type AIReview struct {
	Summary            string   `bson:"summary" json:"summary"`
	Lifestyle          []string `bson:"lifestyle" json:"lifestyle"`
	Nutrition          []string `bson:"nutrition" json:"nutrition"`
	DoctorNeeded       bool     `bson:"doctor_needed" json:"doctor_needed"`
	Urgency            string   `bson:"urgency" json:"urgency"`
	SuggestedSpecialty string   `bson:"suggested_specialty,omitempty" json:"suggested_specialty,omitempty"`
	Disclaimer         string   `bson:"disclaimer" json:"disclaimer"`
	Provider           string   `bson:"provider" json:"provider"`
}

type Analysis struct {
	ID           primitive.ObjectID   `bson:"_id,omitempty" json:"id"`
	OwnerID      primitive.ObjectID   `bson:"owner_id" json:"owner_id"`
	Title        string               `bson:"title" json:"title"`
	Category     string               `bson:"category,omitempty" json:"category,omitempty"`
	LabName      string               `bson:"lab_name,omitempty" json:"lab_name,omitempty"`
	CollectedAt  *time.Time           `bson:"collected_at,omitempty" json:"collected_at,omitempty"`
	OriginalName string               `bson:"original_name" json:"original_name"`
	MimeType     string               `bson:"mime_type" json:"mime_type"`
	StoragePath  string               `bson:"storage_path" json:"-"`
	OCRText      string               `bson:"ocr_text,omitempty" json:"ocr_text,omitempty"`
	Markers      []Marker             `bson:"markers" json:"markers"`
	AIReview     AIReview             `bson:"ai_review" json:"ai_review"`
	Status       string               `bson:"status" json:"status"`
	SharedWith   []primitive.ObjectID `bson:"shared_with" json:"shared_with"`
	CreatedAt    time.Time            `bson:"created_at" json:"created_at"`
	UpdatedAt    time.Time            `bson:"updated_at" json:"updated_at"`
}

type Consultation struct {
	ID                  primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	AnalysisID          primitive.ObjectID `bson:"analysis_id,omitempty" json:"analysis_id,omitempty"`
	PatientID           primitive.ObjectID `bson:"patient_id" json:"patient_id"`
	DoctorID            primitive.ObjectID `bson:"doctor_id,omitempty" json:"doctor_id,omitempty"`
	Source              string             `bson:"source" json:"source"`
	Title               string             `bson:"title" json:"title"`
	Specialty           string             `bson:"specialty,omitempty" json:"specialty,omitempty"`
	ServiceType         string             `bson:"service_type,omitempty" json:"service_type,omitempty"`
	AppointmentAt       *time.Time         `bson:"appointment_at,omitempty" json:"appointment_at,omitempty"`
	PersonalDataConsent bool               `bson:"personal_data_consent" json:"personal_data_consent"`
	MedicalDataConsent  bool               `bson:"medical_data_consent" json:"medical_data_consent"`
	Question            string             `bson:"question" json:"question"`
	Reply               string             `bson:"reply,omitempty" json:"reply,omitempty"`
	Status              string             `bson:"status" json:"status"`
	CreatedAt           time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt           time.Time          `bson:"updated_at" json:"updated_at"`
}

type ScheduleSlot struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DoctorID      primitive.ObjectID `bson:"doctor_id" json:"doctor_id"`
	PatientID     primitive.ObjectID `bson:"patient_id,omitempty" json:"patient_id,omitempty"`
	AppointmentID primitive.ObjectID `bson:"appointment_id,omitempty" json:"appointment_id,omitempty"`
	StartAt       time.Time          `bson:"start_at" json:"start_at"`
	EndAt         time.Time          `bson:"end_at" json:"end_at"`
	Status        string             `bson:"status" json:"status"`
	PatientName   string             `bson:"-" json:"patient_name,omitempty"`
	UpdatedAt     time.Time          `bson:"updated_at" json:"updated_at"`
}

type PatientNote struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DoctorID  primitive.ObjectID `bson:"doctor_id" json:"doctor_id"`
	PatientID primitive.ObjectID `bson:"patient_id" json:"patient_id"`
	Text      string             `bson:"text" json:"text"`
	CreatedAt time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at" json:"updated_at"`
}

type AIMessage struct {
	Role      string    `bson:"role" json:"role"`
	Content   string    `bson:"content" json:"content"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

type AIChat struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DoctorID  primitive.ObjectID `bson:"doctor_id" json:"doctor_id"`
	Title     string             `bson:"title" json:"title"`
	Messages  []AIMessage        `bson:"messages" json:"messages"`
	CreatedAt time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time          `bson:"updated_at" json:"updated_at"`
}

type Guide struct {
	ID          string         `json:"id"`
	Code        string         `json:"code,omitempty"`
	Title       string         `json:"title"`
	Category    string         `json:"category,omitempty"`
	Status      string         `json:"status,omitempty"`
	Developers  []string       `json:"developers,omitempty"`
	Specialties []string       `json:"specialties,omitempty"`
	PublishedAt time.Time      `json:"published_at,omitempty"`
	SourceURL   string         `json:"source_url"`
	Sections    []GuideSection `json:"sections,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type GuideSection struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}
